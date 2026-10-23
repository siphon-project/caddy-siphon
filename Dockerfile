# caddy-siphon — Caddy with the layer4 (caddy-l4) plugin, two DNS providers and rate limiting.
#
# The siphon stack fronts SIP over TLS with Caddy: the layer4 app terminates TLS on a SIP port and
# proxies the decrypted stream to siphon-sip as plain TCP, reusing the same on-demand, per-SNI
# certificate automation Caddy already runs for HTTPS. Stock Caddy has no layer4 app, so this image
# bakes github.com/mholt/caddy-l4 in with xcaddy, along with the modules below. Everything else is
# stock Caddy.
#
# Every version is pinned: the builder image tag pins Caddy, and the module pins fix the rest
# (caddy-l4 warns of breaking changes between versions). Bump them deliberately and re-check the config.
#
# caddy-l4 is built from source with the patches in patches/ applied (the README says what each one
# does and why). The tag is checked against its commit, so a moved tag fails the build instead of
# silently changing the code. The patched module's own tests run before Caddy is built, so a patch
# that no longer applies or no longer passes fails here.
#
# Three more modules come from the Go module proxy, unpatched: the route53 and cloudflare DNS
# providers (ACME DNS-01 challenges, needed for a wildcard certificate) and caddy-ratelimit. Each is
# pinned to a version and the commit that version must resolve to; the commit Go reports for it is
# checked before Caddy is built, so a moved tag fails the build. The go.sum checksum database then
# holds the module contents to that version. caddy-ratelimit is pinned by commit because its one
# tag (v0.1.0) predates the fix for its metrics on a config reload; the README has the detail.

ARG CADDY_VERSION=2.11.4

FROM caddy:${CADDY_VERSION}-builder AS builder
ARG CADDY_L4_VERSION=v0.1.2
ARG CADDY_L4_COMMIT=42db5690dea199f930a6f08005fe2e4aab10dcc9
ARG ROUTE53_VERSION=v1.6.2
ARG ROUTE53_COMMIT=0b76c51234a730e35753dcd0abd1de0707f27eb3
ARG CLOUDFLARE_VERSION=v0.2.4
ARG CLOUDFLARE_COMMIT=a8737d095ad5a48ca031cea6ab704057dbc2d250
ARG RATELIMIT_VERSION=v0.1.1-0.20260612195517-5625512f24f6
ARG RATELIMIT_COMMIT=5625512f24f6f59d6f64fb3aafe5eecff0b286db
COPY patches/ /patches/
RUN git -c advice.detachedHead=false clone --quiet --depth 1 --branch ${CADDY_L4_VERSION} https://github.com/mholt/caddy-l4.git /src/caddy-l4 \
    && test "$(git -C /src/caddy-l4 rev-parse HEAD)" = "${CADDY_L4_COMMIT}" \
    && git -C /src/caddy-l4 apply --verbose /patches/*.patch
RUN cd /src/caddy-l4 && go test ./modules/l4proxy/ ./integration/
# `go mod download -json` reports the commit a version resolves to as Origin.Hash. A missing Origin
# fails the check too, so it cannot pass without having compared anything.
RUN set -e; check() { \
        hash="$(go mod download -json "$1@$2" | sed -n 's/^[[:space:]]*"Hash": "\([0-9a-f]*\)",\{0,1\}$/\1/p')"; \
        test "$hash" = "$3" || { echo "$1@$2 resolves to commit '$hash', expected $3" >&2; exit 1; }; \
        echo "$1@$2 is commit $3"; \
    }; \
    check github.com/caddy-dns/route53 "${ROUTE53_VERSION}" "${ROUTE53_COMMIT}"; \
    check github.com/caddy-dns/cloudflare "${CLOUDFLARE_VERSION}" "${CLOUDFLARE_COMMIT}"; \
    check github.com/mholt/caddy-ratelimit "${RATELIMIT_VERSION}" "${RATELIMIT_COMMIT}"
RUN xcaddy build \
    --with github.com/mholt/caddy-l4=/src/caddy-l4 \
    --with github.com/caddy-dns/route53@${ROUTE53_VERSION} \
    --with github.com/caddy-dns/cloudflare@${CLOUDFLARE_VERSION} \
    --with github.com/mholt/caddy-ratelimit@${RATELIMIT_VERSION}

FROM caddy:${CADDY_VERSION}
COPY --from=builder /usr/bin/caddy /usr/bin/caddy
