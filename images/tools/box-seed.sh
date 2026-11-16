#!/bin/sh
set -eu
seed=/var/lib/outpost-seed
if [ -d "$seed/workspace" ] && [ ! -e /workspace/.outpost/seeded ]; then
  cp -a "$seed/workspace/." /workspace/
  mkdir -p /workspace/.outpost
  touch /workspace/.outpost/seeded
fi
if [ -d "$seed/home" ] && [ ! -e /root/.outpost/home-seeded ]; then
  cp -a "$seed/home/." /root/
  mkdir -p /root/.outpost
  touch /root/.outpost/home-seeded
fi
if [ -f "$seed/images.tar" ] && [ ! -e /var/lib/docker/.outpost-loaded ] && command -v docker >/dev/null 2>&1; then
  i=0
  while ! docker info >/dev/null 2>&1; do
    i=$((i + 1))
    [ "$i" -lt 120 ] || { echo "box-seed: dockerd never answered" >&2; exit 1; }
    sleep 1
  done
  docker load -i "$seed/images.tar"
  touch /var/lib/docker/.outpost-loaded
fi
