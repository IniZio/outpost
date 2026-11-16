#!/bin/sh
# stack.sh up|down: Postgres and the credential proxy for the Go server, as plain containers.
#
# Postgres is ob-pg on 127.0.0.1:5433, with its data in the volume ob-pg-data. The proxy is
# ob-proxy on the boxes network, where boxes reach it as http://ob-proxy:8080, and it fetches
# grants from the server on the host. Its CA lands in $OB_DATA/proxy/ca, which the server reads.
# Nothing here touches the prototype's containers, network or port.
#
# Settings come from the server's env file (~/.config/outpost-go/env): OUTPOST_PROXY_TOKEN and
# OB_PG_PASSWORD, plus OUTPOST_BOX_NETWORK when it is not outpost-boxes.
set -eu
env_file=${OB_ENV:-$HOME/.config/outpost-go/env}
# shellcheck disable=SC1090
. "$env_file"
data=${OB_DATA:-$HOME/.local/share/outpost-go}
network=${OUTPOST_BOX_NETWORK:-outpost-boxes}
proxy_image=${OB_PROXY_IMAGE:-ob-proxy:dev}

up() {
  : "${OB_PG_PASSWORD:?set OB_PG_PASSWORD in $env_file}"
  : "${OUTPOST_PROXY_TOKEN:?set OUTPOST_PROXY_TOKEN in $env_file}"
  mkdir -p "$data/proxy/ca"
  # The agent keeps a network's MTU while a container is attached; 1280 is safe on any link.
  docker network inspect "$network" >/dev/null 2>&1 ||
    docker network create --label dev.outpost.box.network=1 \
      --opt com.docker.network.driver.mtu="${OB_BOX_MTU:-1280}" "$network"
  docker container inspect ob-pg >/dev/null 2>&1 ||
    docker run -d --name ob-pg --restart unless-stopped \
      -e POSTGRES_USER=outpost -e POSTGRES_DB=outpost -e POSTGRES_PASSWORD="$OB_PG_PASSWORD" \
      -p 127.0.0.1:5433:5432 -v ob-pg-data:/var/lib/postgresql/data postgres:17
  docker container inspect ob-proxy >/dev/null 2>&1 ||
    docker run -d --name ob-proxy --restart unless-stopped --network "$network" \
      --add-host host.docker.internal:host-gateway \
      -e OUTPOST_GRANT_URL="${OB_GRANT_URL:-http://host.docker.internal:7400/internal/egress/grant}" \
      -e OUTPOST_PROXY_TOKEN="$OUTPOST_PROXY_TOKEN" \
      -v "$data/proxy/ca:/data/ca" "$proxy_image"
  echo "ob-pg and ob-proxy are up. The proxy CA is $data/proxy/ca/mitmproxy-ca-cert.pem."
}

down() {
  docker rm -f ob-proxy ob-pg >/dev/null 2>&1 || true
  echo "ob-proxy and ob-pg removed; the volume ob-pg-data and $data are kept."
}

case "${1:-}" in
up) up ;;
down) down ;;
*) echo "usage: stack.sh up|down" >&2; exit 2 ;;
esac
