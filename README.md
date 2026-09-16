# caddy-siphon

[Caddy](https://caddyserver.com) with the [layer4](https://github.com/mholt/caddy-l4) app baked in,
for the SIPhon stack.

Stock Caddy fronts the web UI over HTTPS. The SIPhon stack also needs it to front **SIP over TLS**:
the layer4 app terminates TLS on a SIP port and proxies the decrypted stream to the SIP server as
plain TCP, reusing the same on-demand, per-SNI certificate automation Caddy already runs for HTTPS.
Stock Caddy has no layer4 app, so this image adds `github.com/mholt/caddy-l4` with `xcaddy`. Nothing
else changes: it is stock Caddy with one extra app.

## Versions

Both are pinned in the [`Dockerfile`](./Dockerfile), and bumped deliberately:

- **Caddy** — the `caddy:<version>-builder` and runtime image tags.
- **caddy-l4** — the module version. It is pre-1.0 and warns of breaking changes between versions, so
  the config is re-checked on every bump.

## Image

Published to `ghcr.io/siphon-project/caddy-siphon`, built and pushed by CI on a tag. Consumers pin it
by digest.

## Build locally

```
docker build --tag caddy-siphon .
docker run --rm caddy-siphon caddy list-modules | grep layer4
```

The second command should list the `layer4.*` modules, confirming the plugin is present.

## Usage

Run it exactly like stock Caddy (same config, same data and config volumes). A `layer4` app is
configured alongside the `http` app; the layer4 `tls` handler shares the `http` app's certificate
store and automation, so on-demand issuance and per-SNI selection work on the layer4 listener too.
The consuming deployment supplies the Caddyfile / JSON config.
