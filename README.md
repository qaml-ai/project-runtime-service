# Project Runtime Service

Project Runtime Service is a self-hostable compute and filesystem runtime for coding agents.
It runs project containers, keeps project disks persistent, executes commands, exposes file
operations, and provides the storage primitives needed for fast project cloning.

This repository started as the Go sandbox host used by camelAI. The first extraction keeps
the existing behavior intact while moving toward a generic runtime service API that can be
used by other agent products.

## What it provides

- Docker + optional gVisor container lifecycle
- Persistent per-project host directories under `WORKSPACES_ROOT`
- Host-side file operations
- Container exec API
- XFS project quota support
- Project-shaped control API
- Fast reflink clone API on Linux/XFS
- Local tar.gz backups with retention and guarded restore
- Optional bearer or mTLS control-plane authentication
- Generic outbound HTTP proxy capabilities
- R2/S3-style mounted upload/output prefixes
- SQL data proxy sidecar
- Compatibility HTTP proxy hooks for app/platform APIs
- Usage accounting storage

## Target direction

The intended service boundary is:

- The agent application owns users, auth, billing, project metadata, tool schemas, and UI.
- Project Runtime Service owns containers, project disks, quotas, backups, clone, file ops,
  exec, stop/start, disk growth, and project-scoped outbound capabilities.

The project API is generic and product-neutral:

```text
GET  /v1/host/capabilities
GET  /v1/host/stats
GET  /v1/projects/:id
POST /v1/projects/:id/ensure
POST /v1/projects/:id/exec
GET  /v1/projects/:id/fs/read
PUT  /v1/projects/:id/fs/write
GET  /v1/projects/:id/fs/list
DELETE /v1/projects/:id/fs/delete
POST /v1/projects/:id/fs/move
POST /v1/projects/:id/fs/mkdir
GET  /v1/projects/:id/fs/exists
POST /v1/projects/:id/clone
POST /v1/projects/:id/terminate
GET  /v1/projects/:id/backups
POST /v1/projects/:id/backups
POST /v1/projects/:id/restore
GET  /v1/projects/:id/proxies
DELETE /v1/projects/:id
ANY  /p/:capability/*
```

The compatibility API from the original sandbox host is still present while extraction is
underway.

## Storage model

Production Linux defaults:

- `WORKSPACES_ROOT=/srv/sandboxes`
- `SANDBOX_HOST_USAGE_DB_DIR=/srv/sandboxes/.sandbox-host/usage`

Each project or sandbox maps to a leaf directory:

- Host: `/srv/sandboxes/<project-or-sandbox-id>`
- Container bind mount: `/home/claude` in the compatibility runtime

Recommended host mount options:

- XFS on a growable data disk
- `defaults,noatime,prjquota`
- `reflink=1` at filesystem creation time

XFS with reflinks enables fast copy-on-write clones:

```bash
cp -a --reflink=always /srv/sandboxes/source /srv/sandboxes/target.tmp
mv /srv/sandboxes/target.tmp /srv/sandboxes/target
```

## Runtime ports

- `PORT` defaults to `80` on Linux and `4400` on non-Linux. This is the control/API listener.
- `SANDBOX_DOCKER_PROXY_PORT` defaults to `8081`. This is the docker-facing app API proxy listener.
- `DATA_PROXY_PORT` defaults to `8090`. This is the localhost SQL data-proxy sidecar.

## Control-plane authentication

By default the control listener is unauthenticated, which is only appropriate for local
development or private networks.

Bearer auth:

```bash
CONTROL_PLANE_AUTH_TYPE=bearer
CONTROL_PLANE_BEARER_TOKEN=...
```

mTLS auth:

```bash
CONTROL_PLANE_AUTH_TYPE=mtls
CONTROL_PLANE_TLS_CERT_FILE=/etc/project-runtime/server.crt
CONTROL_PLANE_TLS_KEY_FILE=/etc/project-runtime/server.key
CONTROL_PLANE_TLS_CLIENT_CA_FILE=/etc/project-runtime/client-ca.crt
```

Setting TLS files makes the control listener serve HTTPS. When a client CA file is configured,
the listener requires and verifies client certificates.

## Local development

Requires Go 1.24+ and Docker.

```bash
go test ./...
go run ./cmd/sandbox-host
go run ./cmd/data-proxy
```

Useful local defaults:

- `CONTAINER_RUNTIME=runc`
- `WORKSPACES_ROOT=.sandbox-host/workspaces`
- `SANDBOX_HOST_USAGE_DB_DIR=.sandbox-host/usage`
- `CONTAINER_IDLE_TIMEOUT_MS=300000`

## Host setup

Provisioning helpers live in `scripts/`:

- `scripts/setup-host.sh` provisions a Linux host for the current compatibility service.
- `scripts/xfs-project-quota.sh` inspects or updates XFS project quotas.

The setup script still uses some camelAI/chiridion names from the source service. Those are
compatibility details and should be renamed as the extracted service API stabilizes.

## Generic outbound capabilities

The proxy model is inspired by exe.dev integrations: expose named internal network
capabilities to project containers, attach them by project/tag/policy, and inject credentials
outside the user-controlled sandbox.

The sandbox should receive a local capability URL, not provider secrets. The runtime service
should strip spoofable identity headers, resolve the caller project from the container/runtime
identity, inject authoritative headers, and forward the request using a configured auth plugin.

Capabilities are configured with `PROJECT_RUNTIME_PROXY_CAPABILITIES_JSON` or
`PROJECT_RUNTIME_PROXY_CAPABILITIES_FILE`:

```json
{
  "capabilities": [
    {
      "name": "artifacts-main",
      "target": "https://artifacts.example.com",
      "bearerToken": "host-held-token",
      "headers": {
        "X-Integration": "artifacts"
      },
      "allowedProjects": ["project-pizza-delivery", "pizza-delivery"]
    }
  ]
}
```

Containers call the docker-facing proxy listener with `/p/:capability/*`. The host injects
configured credentials and authoritative `X-Project-Runtime-*` identity headers.

When `WORKER_BASE_URL` and either `PROJECT_RUNTIME_PROXY_SECRET` or `SANDBOX_PROXY_SECRET`
are set, the service automatically registers `camelai-artifacts`:

```text
/p/camelai-artifacts/* -> $WORKER_BASE_URL/api/internal/project-runtime/artifacts/*
```

This is the default camelAI Git/Artifacts path. Project checkouts can use that local proxy
remote without receiving Cloudflare Artifacts tokens.

## Backups

Local backups are stored under `PROJECT_RUNTIME_BACKUP_ROOT`, defaulting to
`/srv/sandboxes/.project-runtime/backups` on Linux. Retention defaults to the last 5 backups
per project and can be changed with `PROJECT_RUNTIME_BACKUP_RETENTION`.

Restore extracts the selected archive into a temporary directory first. The current project
directory is only moved aside after extraction succeeds, so a failed restore cannot replace a
valid project with an empty or partially extracted filesystem.

## Disk headroom

`/v1/host/stats` reports total/free bytes and whether the host is above the configured reserve.
`PROJECT_RUNTIME_DISK_RESERVE_BYTES` defaults to 20 GiB. Project ensure, file mutations, clone,
and backup creation return HTTP 507 when the host drops below that reserve.

## License

MIT
