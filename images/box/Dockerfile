FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl wget git gnupg sudo unzip zip xz-utils jq less \
      build-essential python3 python3-venv pipx openssh-server tini \
      docker.io docker-compose-v2 docker-buildx iproute2 procps file rsync openssh-client \
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

COPY entrypoint.sh /usr/local/bin/outpost-box
RUN chmod +x /usr/local/bin/outpost-box && mkdir -p /run/sshd /workspace
WORKDIR /workspace
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/outpost-box"]
