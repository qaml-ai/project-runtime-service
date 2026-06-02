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
- R2/S3-style mounted upload/output prefixes
- SQL data proxy sidecar
- Internal HTTP proxy hooks for app/platform APIs
- Usage accounting storage

## Target direction

The intended service boundary is:

- The agent application owns users, auth, billing, project metadata, tool schemas, and UI.
- Project Runtime Service owns containers, project disks, quotas, backups, clone, file ops,
  exec, stop/start, disk growth, and project-scoped outbound capabilities.

Future project APIs should be generic and product-neutral:

```text
GET  /v1/host/capabilities
POST /v1/projects/:id/ensure
POST /v1/projects/:id/exec
GET  /v1/projects/:id/files/*
PUT  /v1/projects/:id/files/*
POST /v1/projects/:id/clone
POST /v1/projects/:id/stop
POST /v1/projects/:id/proxies
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

The planned proxy model is inspired by exe.dev integrations: expose named internal network
capabilities to project containers, attach them by project/tag/policy, and inject credentials
outside the user-controlled sandbox.

The sandbox should receive a local capability URL, not provider secrets. The runtime service
should strip spoofable identity headers, resolve the caller project from the container/runtime
identity, inject authoritative headers, and forward the request using a configured auth plugin.

Example capability definition:

```json
{
  "id": "artifacts-main",
  "targetBaseUrl": "https://artifacts.cloudflare.net",
  "allowedHosts": ["artifacts.cloudflare.net", "*.artifacts.cloudflare.net"],
  "allowedMethods": ["GET", "POST", "PUT", "PATCH", "DELETE"],
  "auth": {
    "type": "bearer",
    "credentialRef": "workspace.cloudflare_artifacts_token"
  }
}
```

## License

MIT
