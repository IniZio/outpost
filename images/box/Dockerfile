# The box base image: a glibc userland with Docker-in-Docker for a privileged box. Nothing here is
# agent-specific; hc, claude and the box scripts come from the tools image mounted at /opt/outpost.
FROM ubuntu:24.04

ARG BUN_VERSION=1.3.14
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl git gnupg jq unzip xz-utils build-essential \
      docker.io docker-compose-v2 docker-buildx iproute2 procps \
    && rm -rf /var/lib/apt/lists/*

# GitHub CLI from its own apt repo (Ubuntu's is old).
RUN curl -fsSL --retry 5 --retry-all-errors --connect-timeout 20 https://cli.github.com/packages/githubcli-archive-keyring.gpg \
      -o /usr/share/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
      > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update && apt-get install -y --no-install-recommends gh \
    && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL --retry 5 --retry-all-errors --connect-timeout 20 https://mise.run | MISE_INSTALL_PATH=/usr/local/bin/mise sh \
    && echo 'eval "$(mise activate bash --shims)"' > /etc/profile.d/10-mise.sh

# bun, because the groundwork plugin's hooks run on it.
RUN curl -fsSL --retry 5 --retry-all-errors --connect-timeout 20 https://bun.sh/install | BUN_INSTALL=/usr/local bash -s "bun-v${BUN_VERSION}" \
    && bun --version

RUN mkdir -p /workspace
WORKDIR /workspace

# git reads credentials from gh for GitHub, system-wide. gh takes GH_TOKEN (the proxy's placeholder)
# when there is no hosts.yml. The empty helper resets any inherited list.
RUN printf '%s\n' \
      '[credential "https://github.com"]' '	helper =' '	helper = !gh auth git-credential' \
      '[credential "https://gist.github.com"]' '	helper =' '	helper = !gh auth git-credential' \
      > /etc/gitconfig \
    && out=$(printf 'protocol=https\nhost=github.com\n\n' | GH_TOKEN=build-check git credential fill) \
    && echo "$out" | grep -q '^password=build-check$'
