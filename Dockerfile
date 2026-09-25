# caddy-siphon — Caddy with the layer4 (caddy-l4) plugin.
#
# The siphon stack fronts SIP over TLS with Caddy: the layer4 app terminates TLS on a SIP port and
# proxies the decrypted stream to siphon-sip as plain TCP, reusing the same on-demand, per-SNI
# certificate automation Caddy already runs for HTTPS. Stock Caddy has no layer4 app, so this image
# bakes github.com/mholt/caddy-l4 in with xcaddy. Everything else is stock Caddy.
#
# Both versions are pinned: the builder image tag pins Caddy, and the module pin fixes caddy-l4
# (which warns of breaking changes between versions). Bump them deliberately and re-check the config.
#
# caddy-l4 is built from source with the patches in patches/ applied (the README says what each one
# does and why). The tag is checked against its commit, so a moved tag fails the build instead of
# silently changing the code. The patched module's own tests run before Caddy is built, so a patch
# that no longer applies or no longer passes fails here.

ARG CADDY_VERSION=2.11.4

FROM caddy:${CADDY_VERSION}-builder AS builder
ARG CADDY_L4_VERSION=v0.1.2
ARG CADDY_L4_COMMIT=42db5690dea199f930a6f08005fe2e4aab10dcc9
COPY patches/ /patches/
RUN git -c advice.detachedHead=false clone --quiet --depth 1 --branch ${CADDY_L4_VERSION} https://github.com/mholt/caddy-l4.git /src/caddy-l4 \
    && test "$(git -C /src/caddy-l4 rev-parse HEAD)" = "${CADDY_L4_COMMIT}" \
    && git -C /src/caddy-l4 apply --verbose /patches/*.patch
RUN cd /src/caddy-l4 && go test ./modules/l4proxy/ ./integration/
RUN xcaddy build \
    --with github.com/mholt/caddy-l4=/src/caddy-l4

FROM caddy:${CADDY_VERSION}
COPY --from=builder /usr/bin/caddy /usr/bin/caddy
