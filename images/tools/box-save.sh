#!/bin/sh
set -eu
seed=/var/lib/outpost-seed
case "${1:-}" in
prepare)
  rm -rf "$seed"
  mkdir -p "$seed"
  cp -a /workspace "$seed/workspace"
  rm -f "$seed/workspace/.outpost/seeded"
  if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
    images=$(docker image ls --format '{{.Repository}}:{{.Tag}}' | grep -v '<none>' || true)
    # shellcheck disable=SC2086
    [ -z "$images" ] || docker save -o "$seed/images.tar" $images
  fi
  ;;
clean)
  rm -rf "$seed"
  ;;
*)
  echo "usage: box-save prepare|clean" >&2
  exit 2
  ;;
esac
