FROM debian:bookworm-slim AS hc
# hc (herdr-core) is a static binary from the herdr release server. Bump the version and both
# checksums together; the checksums come from https://releases.herdr.dev/core/<version>/manifest.json.
ARG HC_VERSION=0.1.16
ARG HC_SHA256_AMD64=58d6861176dc6fe905b58880d5bff07a9959be613a98e5d66b4583b04b6cfa45
ARG HC_SHA256_ARM64=b6573137d70baff24fb346c957a9879af6bdc4c641188cdf9e47a7b2a677bc92
ARG TARGETARCH
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && apt-get clean
RUN set -eu; mkdir /out; \
    case "$TARGETARCH" in \
      amd64) arch=x86_64; sha=$HC_SHA256_AMD64 ;; \
      arm64) arch=aarch64; sha=$HC_SHA256_ARM64 ;; \
      *) echo "no hc build for $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    curl -fsSL --retry 5 --retry-all-errors -o /hc.tar.gz "https://releases.herdr.dev/core/$HC_VERSION/herdr-core-linux-$arch.tar.gz"; \
    echo "$sha  /hc.tar.gz" | sha256sum -c -; \
    tar -xzf /hc.tar.gz -C /out herdr-core; mv /out/herdr-core /out/hc

FROM debian:bookworm-slim AS claude
ARG CLAUDE_VERSION=stable
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && apt-get clean
RUN curl -fsSL --retry 5 --retry-all-errors https://claude.ai/install.sh | bash -s "$CLAUDE_VERSION" \
    && mkdir /out && cp -L /root/.local/bin/claude /out/claude

FROM scratch AS tools
COPY --chmod=0755 --from=hc /out/hc /opt/outpost/bin/hc
COPY --chmod=0755 box-init.sh /opt/outpost/bin/box-init
COPY --chmod=0755 oom0.sh /opt/outpost/bin/oom0
COPY --chmod=0755 box-seed.sh /opt/outpost/bin/box-seed
COPY --chmod=0755 box-save.sh /opt/outpost/bin/box-save
COPY --chmod=0755 box-trust.sh /opt/outpost/bin/box-trust
COPY --chmod=0755 box-step.sh /opt/outpost/bin/box-step
COPY --chmod=0644 oom-reset.sh /opt/outpost/lib/oom-reset.sh

FROM tools
COPY --chmod=0755 --from=claude /out/claude /opt/outpost/bin/claude
