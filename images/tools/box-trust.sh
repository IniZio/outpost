#!/bin/sh
# box-trust CA_FILE installs the credential proxy's CA in the system bundle, once per CA.
set -eu
src=$1
dst=/usr/local/share/ca-certificates/outpost-proxy.crt
bundle=/etc/ssl/certs/ca-certificates.crt
mark=$(sed -n 2p "$src")
if cmp -s "$src" "$dst" && grep -qxF "$mark" "$bundle" 2>/dev/null; then
  exit 0
fi
mkdir -p "${dst%/*}" "${bundle%/*}"
cp "$src" "$dst"
chmod 0644 "$dst"
if command -v update-ca-certificates >/dev/null 2>&1; then
  update-ca-certificates >/dev/null 2>&1
fi
grep -qxF "$mark" "$bundle" 2>/dev/null || cat "$dst" >>"$bundle"
