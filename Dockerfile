# caddy-siphon — Caddy with the layer4 (caddy-l4) plugin.
#
# The siphon stack fronts SIP over TLS with Caddy: the layer4 app terminates TLS on a SIP port and
# proxies the decrypted stream to siphon-sip as plain TCP, reusing the same on-demand, per-SNI
# certificate automation Caddy already runs for HTTPS. Stock Caddy has no layer4 app, so this image
# bakes github.com/mholt/caddy-l4 in with xcaddy. Everything else is stock Caddy.
#
# Both versions are pinned: the builder image tag pins Caddy, and the module pin fixes caddy-l4
# (which warns of breaking changes between versions). Bump them deliberately and re-check the config.

ARG CADDY_VERSION=2.11.4

FROM caddy:${CADDY_VERSION}-builder AS builder
RUN xcaddy build \
    --with github.com/mholt/caddy-l4@v0.1.2

FROM caddy:${CADDY_VERSION}
COPY --from=builder /usr/bin/caddy /usr/bin/caddy
