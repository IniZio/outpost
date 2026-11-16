#!/bin/sh
set -eu
mkdir -p /run/outpost/tmp
git config --global --add safe.directory '*'
if [ -n "${GH_TOKEN:-}" ]; then gh auth setup-git >/dev/null 2>&1 || true; fi
exec sleep infinity
