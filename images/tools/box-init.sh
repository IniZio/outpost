#!/bin/sh
set -u
PATH=/opt/outpost/bin:$PATH
export PATH

if [ -w /sys/fs/cgroup ] && command -v dockerd >/dev/null 2>&1; then
  mtu=${OUTPOST_MTU:-1500}
  mkdir -p /etc/docker
  printf '{"mtu": %s, "default-network-opts": {"bridge": {"com.docker.network.driver.mtu": "%s"}}}\n' "$mtu" "$mtu" >/etc/docker/daemon.json
  rm -rf /var/run/docker.pid /run/docker /run/containerd
  (echo 0 >/proc/self/oom_score_adj; exec dockerd >/var/log/dockerd.log 2>&1) &
fi

if command -v git >/dev/null 2>&1 && ! git config --global --get-all safe.directory 2>/dev/null | grep -qx '\*'; then
  git config --global --add safe.directory '*'
fi
mkdir -p /run/outpost /root/.config/herdr-core
[ -f /root/.config/herdr-core/config.toml ] || echo 'notes = false' >/root/.config/herdr-core/config.toml
rm -rf "/root/.local/state/herdr-core/${HC_SESSION:-box}"

echo -1000 >/proc/self/oom_score_adj
if ! hc server start >/run/box/hc.log 2>&1; then
  cat /run/box/hc.log >&2
  exit 1
fi
touch /run/box/ready
while :; do sleep 86400; done
