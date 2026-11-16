# Deploying Outpost (Go)

Two shapes, one binary set.

- **One host.** `outpost-go.service`, `outpost-go-agent.service` and `stack.sh up`, as the unit files describe. Postgres, the proxy, the server and the agent all share the host.
- **Cluster plus machines.** The server and its Postgres run on a Kubernetes cluster. Boxes run on attached machines.

## Cluster topology

```
                 https://outpost.example.com
  people, CLI ──────────────┐
                            ▼
                 ┌───────────────────────┐        ┌──────────┐
                 │ ingress (TLS, no      │        │ Postgres │
                 │ idle timeout on WS)   │        │ (external)│
                 └──────────┬────────────┘        └────▲─────┘
                            ▼                          │
                 ┌───────────────────────┐             │
                 │ outpost serve (1 pod) ├─────────────┘
                 └───▲───────────────▲───┘
   wss /machines/link│               │ https /internal/egress/grant (proxy token)
                     │               │
   ┌─────────────────┴───────────────┴──────────────┐
   │ machine: outpost-go-agent (dials out)           │
   │          ob-proxy (mitmproxy) on the boxes net  │
   │          boxes (desk + shell containers)        │
   └─────────────────────────────────────────────────┘
```

- The agent dials out over one WebSocket, so a machine needs no inbound port.
- The proxy on each machine pulls grants from the server's public URL over HTTPS and presents `OUTPOST_PROXY_TOKEN`. The server never dials a machine to give it a credential.
- Every machine's proxy must use the same CA, because the server hands one CA to all boxes. Copy `$OB_DATA/proxy/ca` from the first machine to the others before their first `stack.sh proxy`, and set the chart's `proxy.caPem` to the public certificate in it.

## The server on the cluster

CI publishes, on each push to `go`, at version `0.0.0-go.<run number>`:

| What | Reference |
| --- | --- |
| Server image | `ghcr.io/inizio/outpost:<version>`, also tagged `sha-<commit>` |
| Proxy image | `ghcr.io/inizio/outpost-proxy:<version>` |
| Box base image | `ghcr.io/inizio/outpost-box:<version>`, the chart's `baseImage` |
| Tools image | `ghcr.io/inizio/outpost-tools:<version>`, each machine's `OUTPOST_TOOLS_IMAGE` (hc is pinned in `images/tools/Dockerfile` with checksums) |
| Helm chart | `oci://ghcr.io/inizio/charts/outpost`, version `<version>` |

The chart (`deploy/helm/outpost`) is one Deployment with one replica and a rolling update of `maxSurge: 1, maxUnavailable: 0`. The new pod becomes ready as a standby, the old pod drains its links on SIGTERM, and the lease hands over. `outpost migrate` runs in an init container, and the server then starts with `OUTPOST_MIGRATE=false`. The chart refuses to render when a required value is empty, and the error names the key.

Values an environment must fill:

| Value | Meaning |
| --- | --- |
| `host` | public host name; the server's URL is `https://<host>` and agents dial `wss://<host>/machines/link` |
| `baseImage` | the image every box starts from |
| `secrets.databaseUrl`, `secrets.secretKey`, `secrets.proxyToken` | or `secrets.existingSecret`, a Secret with `OUTPOST_DATABASE_URL`, `OUTPOST_SECRET_KEY`, `OUTPOST_PROXY_TOKEN` |
| `proxy.caPem` | the machines' proxy CA certificate (needed while `proxy.url` is set, which is the default) |
| `allowedLogins` | GitHub logins that may sign in |

Everything else has a default, listed in `values.yaml`. The ingress class defaults to `traefik` and takes no TLS `secretName`. `make chart` lints and renders it.

## A machine

1. Install `outpost-go-agent`, then `outpost-go-agent pair https://outpost.example.com NAME`, and start `outpost-go-agent.service`.
2. Write `~/.config/outpost-go/env` with `OUTPOST_PROXY_TOKEN` (the server's) and `OB_GRANT_URL=https://outpost.example.com/internal/egress/grant`, plus `OB_PROXY_IMAGE=ghcr.io/inizio/outpost-proxy:<version>`. Then run `deploy/stack.sh proxy`.

`env.example` lists every setting.
