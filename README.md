# caddy-siphon

[Caddy](https://caddyserver.com) with the [layer4](https://github.com/mholt/caddy-l4) app baked in,
for the SIPhon stack.

Stock Caddy fronts the web UI over HTTPS. The SIPhon stack also needs it to front **SIP over TLS**:
the layer4 app terminates TLS on a SIP port and proxies the decrypted stream to the SIP server as
plain TCP, reusing the same on-demand, per-SNI certificate automation Caddy already runs for HTTPS.
Stock Caddy has no layer4 app, so this image adds `github.com/mholt/caddy-l4` with `xcaddy`.

It also carries two DNS providers, for ACME DNS-01 challenges (the only way to get a wildcard
certificate), a rate limiting HTTP handler, and a storage module of this repository's own that keeps
Caddy's certificates and ACME state in PostgreSQL (see [PostgreSQL storage](#postgresql-storage)).
Nothing else changes: it is stock Caddy with these modules added.

## Versions

All are pinned in the [`Dockerfile`](./Dockerfile), and bumped deliberately:

| Component | Module | Pin | Commit |
|---|---|---|---|
| Caddy | `caddy:<version>-builder` and runtime image tags | `2.11.4` | |
| [caddy-l4](https://github.com/mholt/caddy-l4) | `layer4` | `v0.1.2`, patched | `42db569` |
| [caddy-dns/route53](https://github.com/caddy-dns/route53) | `dns.providers.route53` | `v1.6.2` | `0b76c51` |
| [caddy-dns/cloudflare](https://github.com/caddy-dns/cloudflare) | `dns.providers.cloudflare` | `v0.2.4` | `a8737d0` |
| [caddy-ratelimit](https://github.com/mholt/caddy-ratelimit) | `http.handlers.rate_limit` | `v0.1.1-0.20260612195517-5625512f24f6` | `5625512` |
| [PostgreSQL storage](./modules/postgresstorage) (this repository) | `caddy.storage.postgresql` | local path | |

The build checks each module version against its commit (`Origin.Hash` from `go mod download -json`
for the three proxy-fetched modules), so a moved tag fails the build. caddy-l4 is pre-1.0 and warns of
breaking changes between versions, so the config is re-checked on every bump.

**caddy-ratelimit is pinned by commit, not by tag.** Its only tag, `v0.1.0` (August 2024), predates
its metrics, the subnet keys (`ipv4_prefix` / `ipv6_prefix`) and the refusal of unknown Caddyfile
subdirectives. The pinned commit is the fix that re-registers its metrics collectors on each config
reload; the commit before it adds metrics without that fix. Move to a tag once upstream cuts one at or
after `5625512`.

## Patches

caddy-l4 is built from source at the pinned tag (checked against its commit) with the patches in
[`patches/`](./patches) applied. The patched module's tests run during the image build.

- **`caddy-l4-proxy-protocol-before-tls.patch`**: adds `proxy_protocol <v1|v2> before_tls`
  (JSON `"proxy_protocol_before_tls": true`) to the layer4 `proxy` handler. Upstream caddy-l4 dials
  a `tls` upstream, completes the handshake, and only then writes the PROXY header, so the header
  travels inside the TLS session. The PROXY protocol specification puts it first on the connection,
  ahead of any other data, and that is where HAProxy (`send-proxy-v2` with `ssl`) and nginx
  (`proxy_protocol on` with `proxy_ssl on`) send it; a server that reads the header before its TLS
  handshake sees a ClientHello instead. With `before_tls` the handler dials plaintext, writes the
  header, then runs the TLS handshake on the same connection. Without it the existing placement is
  unchanged. Based on caddy-l4 `v0.1.2` (`42db569`).

```
proxy {
	proxy_protocol v2 before_tls
	upstream 192.0.2.10:5061 {
		tls
	}
}
```

When bumping caddy-l4, re-check each patch: drop it if upstream has the fix, otherwise rebase it.

## Image

Published to `ghcr.io/siphon-project/caddy-siphon`, built and pushed by CI on a tag. Consumers pin it
by digest.

## Build locally

```
docker build --tag caddy-siphon .
docker run --rm caddy-siphon caddy list-modules | grep -E 'layer4|dns.providers|rate_limit|storage.postgresql'
docker run --rm -v "$PWD/smoke/modules.Caddyfile:/etc/caddy/Caddyfile:ro" caddy-siphon \
	caddy adapt --config /etc/caddy/Caddyfile
smoke/postgres-storage.sh caddy-siphon
```

The second command should list the `layer4.*` modules, `dns.providers.route53`,
`dns.providers.cloudflare`, `http.handlers.rate_limit` and `caddy.storage.postgresql`. The third adapts
[`smoke/modules.Caddyfile`](./smoke/modules.Caddyfile), which uses both DNS providers, a rate limit
zone and the PostgreSQL storage. The fourth starts two Caddy nodes on a throwaway PostgreSQL: the first
issues a certificate from Caddy's internal CA into it, the second must use that certificate rather than
issue its own. CI runs all of them, plus the storage module's tests against a PostgreSQL service:

```
cd modules/postgresstorage
POSTGRES_STORAGE_TEST_DSN='postgres://user:password@127.0.0.1:5432/db?sslmode=disable' go test -race ./...
```

Without `POSTGRES_STORAGE_TEST_DSN` the database tests skip (CI sets `POSTGRES_STORAGE_TEST_REQUIRED=1`,
which turns a skip into a failure).

## Usage

Run it exactly like stock Caddy (same config, same data and config volumes). A `layer4` app is
configured alongside the `http` app; the layer4 `tls` handler shares the `http` app's certificate
store and automation, so on-demand issuance and per-SNI selection work on the layer4 listener too.
The consuming deployment supplies the Caddyfile / JSON config.

## PostgreSQL storage

`caddy.storage.postgresql` keeps everything Caddy's certificate automation (certmagic) stores in
PostgreSQL instead of the local file system: certificates, private keys, ACME accounts, HTTP-01
challenge tokens, and the locks that keep two instances from ordering the same certificate. Several
Caddy instances behind DNS round-robin then share one ACME state: an HTTP-01 challenge is answered by
whichever instance the CA reaches, and a certificate is issued once and served by all of them.

It is written for this image and uses only [pgx](https://github.com/jackc/pgx), which Caddy's own module
graph already contains, at the version Caddy pins.

### Configuration

Caddyfile, as a global option:

```
{
	storage postgresql {
		dsn "{env.CADDY_STORAGE_DSN}"
		table_prefix caddy_   # optional, default caddy_
		lock_ttl 30s          # optional, default 30s, minimum 1s
		auto_create true      # optional, default true
	}
}
```

JSON:

```json
"storage": {
	"module": "postgresql",
	"dsn": "{env.CADDY_STORAGE_DSN}"
}
```

**Pass the DSN as `{env.CADDY_STORAGE_DSN}`, not `{$CADDY_STORAGE_DSN}`.** `{env.*}` is resolved when the
storage is provisioned, so the config holds only the placeholder. `{$*}` is substituted when the
Caddyfile is adapted, so the password ends up in the JSON config, and Caddy writes that to
`autosave.json` in its config directory.

The DSN is a pgx connection string, as a URL (`postgres://user:password@db.example.com:5432/caddy?sslmode=verify-full`)
or key/value pairs (`host=db.example.com user=caddy password=... dbname=caddy sslmode=verify-full`).

### TLS to the database

TLS follows the DSN's `sslmode`. **pgx defaults to `prefer`**: it tries TLS without verifying the
server's certificate and falls back to plaintext if TLS is refused, which protects against neither a
passive nor an active attacker on the path. Use `sslmode=verify-full` (with `sslrootcert=` if the
server's CA is not in the system store) for any database reached over a network.

### Schema and privileges

Two tables, created on provision if missing (`CREATE TABLE IF NOT EXISTS`, under an advisory lock so
instances starting together do not race):

```sql
CREATE TABLE caddy_objects (
	key      text COLLATE "C" PRIMARY KEY,
	value    bytea NOT NULL,
	modified timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE caddy_locks (
	name       text COLLATE "C" PRIMARY KEY,
	owner      text NOT NULL,
	expires_at timestamptz NOT NULL
);
```

The role needs `SELECT`, `INSERT`, `UPDATE` and `DELETE` on both tables. Creating them also needs
`CREATE` on the schema; for a role without it, create the tables beforehand (as above, with your
prefix) and set `auto_create false`, and provisioning then only checks that they are there.

`table_prefix` (lower-case letters, digits and underscores) changes the table names. To give the tables
a schema of their own, put `search_path` in the DSN: pgx sends a parameter it does not know to the
server as a run-time setting, so `...?sslmode=verify-full&search_path=caddy` creates and uses
`caddy.caddy_objects` and `caddy.caddy_locks`.

Keys are stored as certmagic writes them (`certificates/<issuer>/<name>/<name>.crt`). Folders are
implied by the keys under them, as directories are on disk. Every prefix match is a literal
`starts_with()`, never `LIKE`, so `_` and `%` in a name (`wildcard_.example.com`) match only themselves:
cleaning up `wildcard_.example.com` cannot touch `wildcardX.example.com`.

### Locks

certmagic takes a named lock around each certificate operation. Here a lock is a row: its name, a
random owner id, and an expiry by the **database's** clock, so instances with skewed clocks agree.

- **Acquire** is one statement: `INSERT ... ON CONFLICT (name) DO UPDATE ... WHERE expires_at <= now()
  RETURNING owner`. It takes a free or expired lock and leaves a live one alone; PostgreSQL serialises
  the conflicting statements, so two instances never both succeed. `Lock` retries every 250-750 ms
  (jittered) until it succeeds or its context ends; `TryLock` tries once.
- **While held**, a heartbeat extends the expiry every third of `lock_ttl`. A live holder keeps its
  lock however long issuance takes; one that dies or loses the database loses it one `lock_ttl` later,
  and another instance takes over.
- **Unlock** deletes the row only if the owner is still this holder's, so a holder that stalled past
  its TTL cannot release the lock another instance has since taken.
- **RenewLockLease** (certmagic calls it between retries) confirms the lock is still held and extends
  it, but only to `lock_ttl`, not the hours certmagic asks for: the heartbeat already keeps a live
  holder's lock, so a long lease would only matter for a dead holder, and would then block every other
  instance from that certificate for hours.

There are no fencing tokens. An instance cut off from the database for longer than `lock_ttl` keeps
working while another takes over; certmagic tolerates that (it re-checks storage after acquiring a lock)
and the cost is at worst a duplicate ACME order.

### Config reloads

On a reload Caddy provisions the new config's storage, starts the new config, then cleans up the old
one, while an issuance the old config started may still be running: it holds a lock and is about to
store the certificate it ordered, and it will unlock through the old storage. So instances share one
connection pool per DSN, reference-counted. Cleanup gives up only the instance's own reference; each
held lock holds a reference of its own, and its heartbeat keeps running, until it is unlocked. The pool
closes when the last reference goes. With an unchanged DSN a reload never closes it at all.

If the database cannot be reached when Caddy starts or reloads, provisioning fails and Caddy refuses
the config, as it would for any storage it cannot open.
