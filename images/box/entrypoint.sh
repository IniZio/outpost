#!/bin/sh
set -eu
run=/run/outpost

# ssh sessions don't inherit the container's environment; login shells read this.
env | grep -E '^(GH_TOKEN|GITHUB_TOKEN|OUTPOST_[A-Z_]+|ANDROID_[A-Z_]+|JAVA_HOME|TZ|LANG|HTTPS?_PROXY|https?_proxy|NO_PROXY|no_proxy|YARN_HTTPS?_PROXY|NODE_USE_ENV_PROXY|NODE_EXTRA_CA_CERTS|REQUESTS_CA_BUNDLE)=' \
  | sed "s/'/'\\\\''/g; s/=\(.*\)/='\1'/; s/^/export /" > /etc/profile.d/00-outpost.sh || true
chmod 600 /etc/profile.d/00-outpost.sh

if [ -d "$run/files" ]; then
  (cd "$run/files" && find . -type f) | while read -r f; do
    dest="/${f#./}"; mkdir -p "$(dirname "$dest")"; cp "$run/files/$f" "$dest"
    case "$dest" in */.ssh/*) chmod 600 "$dest" ;; esac
  done
fi
if [ -f /usr/local/share/ca-certificates/outpost-proxy.crt ]; then update-ca-certificates >/dev/null 2>&1 || true; fi

if [ -w /sys/fs/cgroup ] && command -v dockerd >/dev/null; then
  mtu=${OUTPOST_MTU:-1500}
  mkdir -p /etc/docker
  printf '{"mtu": %s, "default-network-opts": {"bridge": {"com.docker.network.driver.mtu": "%s"}}}\n' "$mtu" "$mtu" > /etc/docker/daemon.json
  rm -rf /var/run/docker.pid /run/docker /run/containerd
  (dockerd >/var/log/dockerd.log 2>&1 &)
fi

mkdir -p /root/.ssh
cp "$run/ssh/id.pub" /root/.ssh/authorized_keys
chmod 700 /root/.ssh && chmod 600 /root/.ssh/authorized_keys
[ -f /etc/ssh/ssh_host_ed25519_key ] || ssh-keygen -q -A
git config --global --add safe.directory '*'
if [ -n "${GH_TOKEN:-}" ]; then
  gh auth setup-git >/dev/null 2>&1 || true
  if ! git config --global user.email >/dev/null; then
    who=$(timeout 20 gh api user --jq '[.id, .login, (.name // .login)] | @tsv' 2>/dev/null || true)
    if [ -n "$who" ]; then
      printf '%s\n' "$who" | while IFS="$(printf '\t')" read -r id login name; do
        git config --global user.name "$name"
        git config --global user.email "$id+$login@users.noreply.github.com"
      done
    fi
  fi
fi

touch "$run/box-ready"
exec /usr/sbin/sshd -D -e -p 2222 -o ListenAddress=127.0.0.1 \
  -o PasswordAuthentication=no -o PermitRootLogin=prohibit-password \
  -o AcceptEnv=none
