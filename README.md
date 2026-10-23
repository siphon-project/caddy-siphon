# caddy-siphon

[Caddy](https://caddyserver.com) with the [layer4](https://github.com/mholt/caddy-l4) app baked in,
for the SIPhon stack.

Stock Caddy fronts the web UI over HTTPS. The SIPhon stack also needs it to front **SIP over TLS**:
the layer4 app terminates TLS on a SIP port and proxies the decrypted stream to the SIP server as
plain TCP, reusing the same on-demand, per-SNI certificate automation Caddy already runs for HTTPS.
Stock Caddy has no layer4 app, so this image adds `github.com/mholt/caddy-l4` with `xcaddy`.

It also carries two DNS providers, for ACME DNS-01 challenges (the only way to get a wildcard
certificate), and a rate limiting HTTP handler. Nothing else changes: it is stock Caddy with these
modules added.

## Versions

All are pinned in the [`Dockerfile`](./Dockerfile), and bumped deliberately:

| Component | Module | Pin | Commit |
|---|---|---|---|
| Caddy | `caddy:<version>-builder` and runtime image tags | `2.11.4` | |
| [caddy-l4](https://github.com/mholt/caddy-l4) | `layer4` | `v0.1.2`, patched | `42db569` |
| [caddy-dns/route53](https://github.com/caddy-dns/route53) | `dns.providers.route53` | `v1.6.2` | `0b76c51` |
| [caddy-dns/cloudflare](https://github.com/caddy-dns/cloudflare) | `dns.providers.cloudflare` | `v0.2.4` | `a8737d0` |
| [caddy-ratelimit](https://github.com/mholt/caddy-ratelimit) | `http.handlers.rate_limit` | `v0.1.1-0.20260612195517-5625512f24f6` | `5625512` |

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
docker run --rm caddy-siphon caddy list-modules | grep -E 'layer4|dns.providers|rate_limit'
docker run --rm -v "$PWD/smoke/modules.Caddyfile:/etc/caddy/Caddyfile:ro" caddy-siphon \
	caddy adapt --config /etc/caddy/Caddyfile
```

The second command should list the `layer4.*` modules, `dns.providers.route53`,
`dns.providers.cloudflare` and `http.handlers.rate_limit`. The third adapts
[`smoke/modules.Caddyfile`](./smoke/modules.Caddyfile), which uses both DNS providers and a rate limit
zone; CI runs both checks.

## Usage

Run it exactly like stock Caddy (same config, same data and config volumes). A `layer4` app is
configured alongside the `http` app; the layer4 `tls` handler shares the `http` app's certificate
store and automation, so on-demand issuance and per-SNI selection work on the layer4 listener too.
The consuming deployment supplies the Caddyfile / JSON config.
