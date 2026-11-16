FROM node:24-bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl git openssh-client ripgrep jq unzip less procps tini \
    && rm -rf /var/lib/apt/lists/*
RUN curl -fsSL --retry 5 --retry-all-errors --connect-timeout 20 https://cli.github.com/packages/githubcli-archive-keyring.gpg \
      -o /usr/share/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
      > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update && apt-get install -y --no-install-recommends gh \
    && rm -rf /var/lib/apt/lists/*

# Claude Code and bun (plugin hooks often run TypeScript with bun).
RUN npm install -g @anthropic-ai/claude-code && npm cache clean --force
COPY --from=oven/bun:1.3.14-slim /usr/local/bin/bun /usr/local/bin/bun
RUN ln -s bun /usr/local/bin/bunx

COPY box-shell outpost-profile outpost-seed-claude /usr/local/bin/
COPY entrypoint.sh /usr/local/bin/outpost-desk
RUN chmod +x /usr/local/bin/box-shell /usr/local/bin/outpost-* && mkdir -p /workspace

ENV IS_SANDBOX=1 \
    CLAUDE_CODE_SHELL_PREFIX=/usr/local/bin/box-shell \
    TMPDIR=/run/outpost/tmp \
    DISABLE_AUTOUPDATER=1
WORKDIR /workspace
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/outpost-desk"]
