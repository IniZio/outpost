FROM debian:bookworm-slim AS claude
ARG CLAUDE_VERSION=stable
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
RUN curl -fsSL --retry 5 https://claude.ai/install.sh | bash -s "$CLAUDE_VERSION" \
    && mkdir /out && cp -L /root/.local/bin/claude /out/claude

FROM scratch AS tools
COPY --chmod=0755 hc /opt/outpost/bin/hc
COPY --chmod=0755 box-init.sh /opt/outpost/bin/box-init
COPY --chmod=0755 oom0.sh /opt/outpost/bin/oom0
COPY --chmod=0755 box-seed.sh /opt/outpost/bin/box-seed
COPY --chmod=0755 box-save.sh /opt/outpost/bin/box-save
COPY --chmod=0755 box-trust.sh /opt/outpost/bin/box-trust
COPY --chmod=0644 oom-reset.sh /opt/outpost/lib/oom-reset.sh

FROM tools
COPY --chmod=0755 --from=claude /out/claude /opt/outpost/bin/claude
