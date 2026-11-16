#!/bin/sh
set -eu
run=/run/outpost

# ssh sessions don't inherit the container's environment; login shells read this.
env | grep -E '^(GH_TOKEN|GITHUB_TOKEN|OUTPOST_[A-Z_]+|ANDROID_[A-Z_]+|JAVA_HOME|TZ|LANG)=' \
  | sed "s/'/'\\\\''/g; s/=\(.*\)/='\1'/; s/^/export /" > /etc/profile.d/00-outpost.sh || true

if [ -d "$run/files" ]; then
  (cd "$run/files" && find . -type f) | while read -r f; do
    dest="/${f#./}"; mkdir -p "$(dirname "$dest")"; cp "$run/files/$f" "$dest"
    case "$dest" in */.ssh/*) chmod 600 "$dest" ;; esac
  done
fi

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
if [ -n "${GH_TOKEN:-}" ]; then gh auth setup-git >/dev/null 2>&1 || true; fi

touch "$run/box-ready"
exec /usr/sbin/sshd -D -e -p 2222 -o ListenAddress=127.0.0.1 \
  -o PasswordAuthentication=no -o PermitRootLogin=prohibit-password \
  -o AcceptEnv=none
