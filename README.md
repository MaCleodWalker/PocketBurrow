# PocketBurrow

Tenant-sharded PocketBase hosting: one PocketBase container and persistent volume
per tenant, with a hostname-routing gateway and a small management API.

**This is a single-host MVP, not a distributed PocketBase database.** It provides
the provisioning and routing foundation for spreading independent tenants across
hosts. A single tenant still has one SQLite writer and runs on one machine.

## Architecture

```text
Clients -> gateway :8080 -> acme.example.com -> PocketBase acme -> acme volume
                        -> beta.example.com -> PocketBase beta -> beta volume

Operator -> authenticated admin API :9090 -> durable tenant registry
                                         -> Docker Engine (Unix socket)
```

- Each tenant has independent records, auth users, files, schema and realtime
  subscriptions. No cross-tenant queries or shared user identity are provided.
- The gateway forwards PocketBase paths, query strings and authorization headers.
  It streams realtime SSE responses without buffering.
- Named Docker volumes store `/pb/pb_data`; restarting PocketBurrow does not
  remove containers or data.
- Container ports bind only to host loopback. Both PocketBurrow listeners also
  default to loopback. Run PocketBurrow **on the Docker host**, not inside a bridge
  network container; the upstream addresses refer to the host's loopback.
- The registry uses atomic file replacement and fsync, with an exclusive process
  lock. Only one control-plane process may use a registry directory.
- Provisioning persists intent before touching Docker. Failed operations remain
  visible as `error`; interrupted operations remain `provisioning`. Retry with
  the tenant's `start` endpoint, which reuses its container and volume.
- Startup reconciles ready tenants' Docker port bindings. If reconciliation fails,
  startup fails rather than routing to potentially stale ports.

## Requirements

- Linux, Go 1.24+, and a local Docker Engine supporting API v1.45 (Docker 26+).
- Permission to access the Docker socket. This is effectively host-root access:
  run the control plane only under a trusted service account. Do not make the
  socket world-writable.
- A PocketBase image pulled from GitHub Container Registry (GHCR) or built locally.
  PocketBase has no official Docker image.
  This repository pins PocketBase 0.40.5; review upstream compatibility and updates
  before deploying.

## Quick start

### Pull a published image from GHCR

```sh
export BURROW_PB_IMAGE=ghcr.io/macleodwalker/pocketburrow-pocketbase:0.40.5
docker pull "$BURROW_PB_IMAGE"
```

Public images can be pulled without signing in. For a private image, first run
`docker login ghcr.io --username your-github-username` and enter a personal access
token (classic) with `read:packages` and access to the package at the password
prompt, then run the pull command above.

PocketBurrow does not automatically pull missing images. Pull the image on the
same Docker Engine it will manage, and keep `BURROW_PB_IMAGE` exported in the
terminal where you start PocketBurrow. Use an image supporting your host's
architecture; for production, prefer an immutable digest reference such as
`ghcr.io/macleodwalker/pocketburrow-pocketbase@sha256:<published-digest>`.

### Alternative: build the image locally

If no GHCR package has been published yet, build the supplied Dockerfile instead:

```sh
export BURROW_PB_IMAGE=pocketburrow/pocketbase:0.40.5
docker build -f docker/pocketbase.Dockerfile \
  -t "$BURROW_PB_IMAGE" .
```

The Dockerfile supports `TARGETARCH=amd64` or `arm64`; set `--build-arg
TARGETARCH=arm64` when building natively on ARM without BuildKit's automatic args.
To customize schemas/hooks, build a derivative image with `pb_migrations` and
`pb_hooks` under `/pb`, and set `BURROW_PB_IMAGE` before provisioning.

### Build and start PocketBurrow

After pulling or building the image, build the gateway, generate an admin token
and start the process in the same terminal:

```sh
go build -o pocketburrow ./cmd/pocketburrow
export BURROW_ADMIN_TOKEN="$(openssl rand -hex 32)"
./pocketburrow
```

In another terminal, set the same token in `BURROW_ADMIN_TOKEN`, then:

```sh
curl --fail-with-body http://127.0.0.1:9090/v1/tenants \
  -H "Authorization: Bearer $BURROW_ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"id":"acme"}'

curl --fail-with-body http://127.0.0.1:8080/api/health \
  -H 'Host: acme.localhost'

curl --fail-with-body http://127.0.0.1:9090/v1/tenants \
  -H "Authorization: Bearer $BURROW_ADMIN_TOKEN"
```

Successful creation returns HTTP 201 and:

```json
{
  "id": "acme",
  "host": "acme.localhost",
  "state": "ready",
  "endpoint": "http://127.0.0.1:49152",
  "created_at": "2026-10-08T20:00:00Z"
}
```

The endpoint and timestamp above are examples. Docker chooses the actual port.
Creation waits for PocketBase's health endpoint before marking the tenant ready.

Provision a superuser before exposing the tenant publicly:

```sh
docker exec -it pocketburrow-acme /pb/pocketbase superuser create \
  admin@example.com 'replace-with-a-strong-unique-password'
```

Then access `http://acme.localhost:8080/_/` using a browser that resolves
`*.localhost`, or configure a local DNS/hosts entry. PocketBurrow does not create
superusers or configure PocketBase collection access rules.

## Management API

All `/v1` endpoints require `Authorization: Bearer <BURROW_ADMIN_TOKEN>`.

| Method | Path | Behavior |
| --- | --- | --- |
| GET | `/healthz` | Unauthenticated process liveness, not tenant/Docker readiness |
| GET | `/v1/tenants` | Sorted tenant array, including provisioning errors |
| POST | `/v1/tenants` | Create from `{"id":"acme"}`; duplicate IDs return 409 |
| POST | `/v1/tenants/{id}/start` | Recover provisioning or start/reconcile an existing tenant |
| GET | `/v1/tenants/{id}/health` | Live PocketBase health check; unhealthy returns 503 |

Tenant IDs are lowercase DNS labels, 1-63 characters. Arbitrary upstream URLs,
images and volume paths cannot be supplied via the API. Errors return
`{"error":"..."}` and server failures are logged. Provisioning is serialized in
this first version, while proxy traffic continues independently.

## Configuration

| Environment variable | Default |
| --- | --- |
| `BURROW_ADMIN_TOKEN` | Required, minimum 32 characters |
| `BURROW_BASE_DOMAIN` | `localhost` |
| `BURROW_GATEWAY_ADDR` | `127.0.0.1:8080` |
| `BURROW_ADMIN_ADDR` | `127.0.0.1:9090` |
| `BURROW_DATA_DIR` | `./data` |
| `BURROW_DOCKER_SOCKET` | `/var/run/docker.sock` |
| `BURROW_PB_IMAGE` | `pocketburrow/pocketbase:0.40.5` |

The base domain cannot change while the registry contains tenants. Existing
containers with mismatched ownership labels, image names, port bindings or volume
mounts are not adopted. Existing volumes with mismatched ownership are rejected.
Changing the image does **not** upgrade existing tenants.

Per-container defaults: 512 MiB RAM, one CPU, 128 PIDs, dropped Linux
capabilities, no privilege escalation, and rotated Docker logs. These limits
are not capacity planning; benchmark tenant workloads and size the host.

## Deployment boundaries

- Terminate TLS at a trusted edge proxy such as Caddy. Route wildcard tenant
  domains to the gateway and preserve the original Host. Never route public
  traffic to the management API.
- The gateway does not trust incoming forwarded headers. It replaces them using
  its direct peer and connection scheme, so behind an edge proxy PocketBase sees
  the edge IP and an HTTP internal hop, not the original client's IP/TLS scheme.
  Trusted-edge configuration is future work; do not use these headers as an
  authorization signal.
- Tenant hostnames are routing keys, not authentication. PocketBase's own
  collection rules and authentication control access. Containers provide
  process/storage separation, not a hardened sandbox for untrusted tenant code.
- There is no automatic failover, replication, autoscaling, image upgrade,
  deletion, backup scheduler or tenant migration. Named volumes are not backups.
  Use PocketBase's backup API or stop a tenant before copying its data. Back up
  the registry separately; losing it loses the routing/provisioning inventory.
- Containers restart through Docker's `unless-stopped` policy. A manually
  removed/recreated container requires `start` to refresh its endpoint; there is
  no background reconciliation loop yet.
- Registry storage must be on a local Linux filesystem with atomic rename and
  fsync support. Do not share SQLite files or this registry across machines.
- Long-lived SSE connections are retained during normal operation. Shutdown
  allows ten seconds to drain, then closes remaining connections; clients should
  reconnect. Tenant provisioning retries briefly interrupt that tenant's routing.

## Multi-host evolution

The Go `Runtime` interface is the integration boundary for node agents. A
multi-host version must add:

1. Authenticated, mutually trusted node-agent RPC and private network transport.
2. Tenant-to-node placement, resource inventory and persistent operation IDs.
3. Gateway reachability to private node endpoints (the MVP intentionally only
   accepts loopback upstreams).
4. Periodic reconciliation and controller leader election with shared durable
   metadata.
5. Safe migration: quiesce writes, back up/copy tenant state, restore and verify,
   atomically switch placement, then retire the old writer.

Never run two active writers against the same tenant volume. Adding nodes
increases aggregate capacity across tenants; it does not split one tenant's
database.

## Validation

```sh
go test -race ./...
go vet ./...
```

Tests cover tenant routing isolation, forwarded-header sanitization,
authorization, input validation, streaming SSE, provisioning retries, Docker API
configuration/idempotency, registry locking/persistence and restart reconciliation.
The Docker tests use a fake Engine API; the quick-start commands above are the
real-Docker smoke test.

References: [PocketBase FAQ](https://pocketbase.io/faq/) and
[production guide](https://pocketbase.io/docs/going-to-production/).
