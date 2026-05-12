# Transit Ops Center

[![Build and deploy](https://github.com/rezaparvasidev/transit-ops-center/actions/workflows/deploy.yml/badge.svg)](https://github.com/rezaparvasidev/transit-ops-center/actions/workflows/deploy.yml)

Live operations dashboard for Bay Area public transit — real-time vehicle positions from BART, SF Muni, Caltrain, and AC Transit rendered on an interactive map.

**Live demo:** https://transit-ops.victoriousbush-871e8768.eastus.azurecontainerapps.io

## Architecture (v0)

```
┌─────────────────┐   GTFS-RT       ┌──────────────────┐   SSE     ┌──────────────────┐
│  511.org        │ ──protobuf──▶   │  Go service     │ ──json──▶ │  SvelteKit + UI  │
│  (regional      │   every 15s     │  • polls feed   │   every   │  • MapLibre GL   │
│   transit hub)  │                 │  • in-mem store │    2s     │  • live markers  │
└─────────────────┘                 │  • SSE + REST   │           │  • route panel   │
                                    └──────────────────┘           └──────────────────┘
                                              │
                                              ▼
                          Single container, distroless, multi-stage Docker build
                                              │
                                              ▼
                          Azure Container Registry → Azure Container Apps
```

## Tech stack

| Layer | Choice | Reason |
|---|---|---|
| Ingest | Go 1.22 + `MobilityData/gtfs-realtime-bindings` | Concurrency, small static binary, official protobuf bindings |
| Transport | Server-Sent Events (SSE) | One-way live stream; simpler than WebSocket + GraphQL for v0 |
| State | In-memory `map[string]Vehicle` behind sync.RWMutex | Sub-ms reads; no DB needed until we add persistent operator actions |
| Frontend | SvelteKit 2 + Svelte 5 (runes) + Tailwind | Compiler-based reactivity — smaller bundle, less ceremony than React |
| Map | MapLibre GL JS | Open-source vector map; free demo tiles; pans/zooms thousands of markers smoothly |
| Build | Multi-stage Dockerfile → distroless | Build deps stay out of runtime; no shell in production image |
| Registry | Azure Container Registry (shared) | Private, native AAD auth, integrates with ACA |
| Compute | Azure Container Apps | Built-in HTTPS, scale-to-zero capable, no Kubernetes management |

## Run locally

```sh
# 1. Get a free API token from https://511.org/open-data/token
# 2. Build and run
docker build -t transit-ops:dev .
docker run --rm -p 8080:8080 \
  -e TRANSIT_API_KEY=your_token \
  -e TRANSIT_AGENCY=SF \
  transit-ops:dev
# 3. Open http://localhost:8080
```

`TRANSIT_AGENCY` codes: `SF` (Muni), `BA` (BART), `CT` (Caltrain), `AC` (AC Transit), `RG` (regional aggregate of all operators).

## Deploy to Azure

```powershell
# Push image
az acr login -n <your-acr>
docker tag transit-ops:dev <your-acr>.azurecr.io/transit-ops:v0
docker push <your-acr>.azurecr.io/transit-ops:v0

# Deploy (admin creds; switch to managed identity for production)
$creds = az acr credential show -n <your-acr> | ConvertFrom-Json
az containerapp create `
  -n transit-ops -g <your-rg> `
  --environment <your-aca-env> `
  --image <your-acr>.azurecr.io/transit-ops:v0 `
  --target-port 8080 --ingress external `
  --registry-server <your-acr>.azurecr.io `
  --registry-username $creds.username `
  --registry-password $creds.passwords[0].value `
  --secrets "transit-api-key=$env:TRANSIT_API_KEY" `
  --env-vars "TRANSIT_API_KEY=secretref:transit-api-key" "TRANSIT_AGENCY=SF" `
  --min-replicas 1 --max-replicas 1 --cpu 0.5 --memory 1.0Gi
```

## CI/CD

Every push to `main` triggers `.github/workflows/deploy.yml`, which:

1. Logs into Azure via **OIDC federated identity** — no long-lived secrets stored in GitHub.
2. Authenticates to ACR with the federated token.
3. Builds the multi-stage image and tags it with the commit SHA.
4. Pushes the image to ACR.
5. Updates the Container App with `az containerapp update --image`.
6. Polls `/healthz` for up to 60s to verify the new revision serves traffic.

```
push to main ─▶ GitHub Actions runner ─OIDC─▶ Azure App Registration
                                              │
                                              ▼ AcrPush role on rezaportfolioacr
                              docker build ─▶ docker push ─▶ ACR
                                              │
                                              ▼ Contributor role on transit-ops Container App
                                            az containerapp update --image
                                              │
                                              ▼ curl /healthz x12 with 5s backoff
                                            verify or fail
```

Roles granted to the SP follow least-privilege:
- `AcrPush` scoped to `rezaportfolioacr` only
- `Contributor` scoped to the `transit-ops` Container App only (not the whole RG)

No subscription-wide permissions, no resource-group-wide permissions, no client secrets.

## v1 roadmap

v0 is a deliberate cut. Future increments map to the components of a production transit operations platform:

1. **Split processes** — extract a dedicated `ingester` from the `api` server; introduce **NATS JetStream** as the broker (subjects `vehicles.>`). Enables multi-replica fan-out and durable replay.
2. **GraphQL + subscriptions** — replace REST/SSE with `gqlgen` + `graphql-ws`. Typed schema + bidirectional channel needed for mutations.
3. **Postgres + golang-migrate** — persist `operator_actions` (flags, notes, history). Static GTFS schedule cached for routes/stops/shapes.
4. **Alerts service** — anomaly detection (route bunching, stop dwell-time violations) over the NATS stream.
5. **Human-in-the-loop UI** — `flagVehicle` mutation, history table, fan-out to all connected clients in <1s.
6. **Map polish** — D3.js Albers projection + TopoJSON route polylines + smooth marker interpolation.
7. **AKS migration** — Helm charts, NGINX ingress, cert-manager + Let's Encrypt, custom domain.
8. **Bazel monorepo** — hermetic builds, remote caching, `rules_oci` for images.
9. **Observability** — OpenTelemetry tracing, Azure Monitor exporter, structured logs.
10. **CI/CD** — GitHub Actions with OIDC federated identity to Azure (no long-lived secrets).

## Key design decisions (interview-defensible)

- **511.org over per-agency feeds:** BART doesn't publish GTFS-RT vehicle positions. 511.org is the official MTC aggregator and serves protobuf VehiclePosition feeds for every Bay Area operator behind one key.
- **SSE over WebSocket:** v0 flow is server→client only. SSE has automatic reconnection and is one less moving part. Easy to swap to WebSocket when we add mutations.
- **min=max=1 replicas:** in-memory state is per-replica. The right scale-out design needs NATS as a shared bus first (see roadmap item 1). v0 deliberately keeps it single-replica.
- **Distroless runtime:** no shell, no package manager, runs as `nonroot`. Minimal attack surface; image size ~15 MB.
- **Static SvelteKit served by Go:** one binary, one port, one container. v1 splits into separate frontend/backend services.
