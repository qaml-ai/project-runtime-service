# Project Runtime Service

Project Runtime Service is a self-hostable compute and filesystem runtime for coding agents.
It runs project containers, keeps project disks persistent, executes commands, exposes file
operations, and provides the storage primitives needed for fast project cloning.

## What it provides

- Docker + optional gVisor container lifecycle
- Persistent per-project host directories under `WORKSPACES_ROOT`
- Host-side file operations
- Container exec API
- ZFS per-project datasets with refquota support
- XFS project quota support for directory-mode hosts
- Project-shaped control API
- Fast project clone API using ZFS snapshots/clones, or XFS reflinks in directory mode
- S3-compatible backups with retention and guarded restore
- Optional S3-compatible project archival to move inactive project files off local disk
- Optional bearer or mTLS control-plane authentication
- Generic outbound HTTP proxy capabilities
- R2/S3-style mounted upload/output prefixes
- SQL data proxy sidecar
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
GET  /v1/projects/:id/storage
POST /v1/projects/:id/archive
POST /v1/projects/:id/unarchive
GET  /v1/projects/:id/proxies
DELETE /v1/projects/:id
ANY  /p/:capability/*
```

## Storage model

Production Linux defaults:

- `WORKSPACES_ROOT=/srv/project-runtime`
- `PROJECT_RUNTIME_USAGE_DB_DIR=/srv/project-runtime/.project-runtime/usage`

Each project maps to a leaf directory:

- Host: `/srv/project-runtime/<project-id>`
- Container bind mount: configurable, defaulting to `/workspace`

Recommended production storage backend:

- ZFS pool on a growable data disk
- one child dataset per project
- `refquota` for per-project size limits
- ZFS snapshots and clones for fast project cloning
- `zfs send` streams for backups and cold-storage archives

Minimal ZFS config:

```text
PROJECT_RUNTIME_STORAGE_DRIVER=zfs
PROJECT_RUNTIME_ZFS_POOL=projectpool
PROJECT_RUNTIME_ZFS_DATASET_PREFIX=projects
PROJECT_RUNTIME_ZFS_REFQUOTA=100g
```

In ZFS mode, each project maps to:

- Dataset: `<pool>/<prefix>/<project-runtime-name>`
- Mountpoint: `/srv/project-runtime/<project-runtime-name>`
- Container bind mount: configurable, defaulting to `/workspace`

For Azure Premium SSD v2 hosts, set ZFS auto-expand on the pool:

```bash
sudo zpool set autoexpand=on projectruntime
```

`autoexpand=on` lets ZFS consume newly-added capacity after the managed disk is
resized. To trigger Azure disk growth automatically, install
`scripts/grow-azure-zfs-disk.sh` as a root systemd timer and give the VM's managed
identity `Contributor` scoped only to the project-runtime managed disk. Example
timer config:

```text
AZURE_SUBSCRIPTION_ID=...
AZURE_RESOURCE_GROUP=rg-chiridion-sandbox-prod
AZURE_DISK_NAME=datadisk-project-runtime-prod
AZURE_DISK_LUN=1
ZPOOL_NAME=projectruntime
MIN_FREE_BYTES=107374182400
GROW_AT_CAP_PERCENT=85
GROW_INCREMENT_GB=1024
MAX_DISK_GB=10240
```

Directory mode remains available for simpler hosts. Recommended directory-mode mount
options:

- XFS on a growable data disk
- `defaults,noatime,prjquota`
- `reflink=1` at filesystem creation time

XFS with reflinks enables copy-on-write directory clones:

```bash
cp -a --reflink=always /srv/project-runtime/source /srv/project-runtime/target.tmp
mv /srv/project-runtime/target.tmp /srv/project-runtime/target
```

## Runtime image

The service is image-agnostic. Container images only need to satisfy a small runtime
contract:

- accept a bind-mounted workspace directory
- have a user that can run shell commands
- include whatever tools your agent needs
- optionally expose `GET /health` on port `8080` for faster readiness checks

The preferred generic config keys are:

```text
PROJECT_RUNTIME_IMAGE=project-runtime-basic:latest
PROJECT_RUNTIME_CONTAINER_USER=runtime
PROJECT_RUNTIME_CONTAINER_HOME=/workspace
PROJECT_RUNTIME_CONTAINER_WORKDIR=/workspace
PROJECT_RUNTIME_WORKSPACE_MOUNT=/workspace
PROJECT_RUNTIME_FILE_OWNER_UID=1001
PROJECT_RUNTIME_FILE_OWNER_GID=1001
```

`Dockerfile.basic` is the boring quickstart image. It includes bash, git, curl, jq,
ripgrep, archive tools, and a tiny health listener. Product-specific images can layer on
agent skills, deploy tooling, language runtimes, browsers, or package managers without
changing the service protocol.

## Runtime ports

- `PORT` defaults to `80` on Linux and `4400` on non-Linux. This is the control/API listener.
- `PROJECT_RUNTIME_DOCKER_PROXY_PORT` defaults to `8081`. This is the docker-facing app API proxy listener.
- `DATA_PROXY_PORT` defaults to `8090`. This is the localhost SQL data-proxy sidecar.

## App API proxy

Project containers use the docker-facing proxy listener for Cloudflare API and app
service calls. The runtime service does not mint or pass per-deploy tokens into
containers. It forwards to `WORKER_BASE_URL`, injects `X-Project-Runtime-Secret`
when `PROJECT_RUNTIME_PROXY_SECRET` is configured, and always injects
authoritative `X-Chiridion-*` identity headers for the target project.

The app Worker can authenticate the runtime with that shared secret or with mTLS.
Either way, project identity comes from the runtime-injected headers, not from
user-controlled container headers.

Containers should point deploy tooling at the static docker-facing endpoint:
`http://host.docker.internal:8081/deploy/client/v4`. The runtime resolves the caller
container and injects the project identity from host-side container metadata.

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

Outbound mTLS for Worker proxy callbacks:

```bash
PROJECT_RUNTIME_MTLS_CLIENT_CERT_FILE=/etc/qaml-project-runtime/mtls/client.crt
PROJECT_RUNTIME_MTLS_CLIENT_KEY_FILE=/etc/qaml-project-runtime/mtls/client.key
```

When these are set, runtime-originated HTTPS calls to configured proxy capabilities
and deploy/artifacts endpoints present the client certificate.

## Local development

Requires Go 1.24+ and Docker.

```bash
go test ./...
go run ./cmd/project-runtime
go run ./cmd/data-proxy
```

Useful local defaults:

- `CONTAINER_RUNTIME=runc`
- `WORKSPACES_ROOT=.project-runtime/workspaces`
- `PROJECT_RUNTIME_USAGE_DB_DIR=.project-runtime/usage`
- `CONTAINER_IDLE_TIMEOUT_MS=300000`

## Host setup

Provisioning helpers live in `scripts/`:

- `scripts/setup-host.sh` provisions a Linux host.
- `scripts/xfs-project-quota.sh` inspects or updates XFS project quotas.

## Generic outbound capabilities

The proxy model is inspired by exe.dev integrations: expose named internal network
capabilities to project containers, attach them by project/tag/policy, and inject credentials
outside the user-controlled container.

The container should receive a local capability URL, not provider secrets. The runtime service
should strip spoofable identity headers, resolve the caller project from the container/runtime
identity, inject authoritative headers, and forward the request using a configured auth plugin.

Capabilities are configured with a JSON file. On Linux the default path is
`/etc/project-runtime-service/proxies.json`; override it with
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

Application-specific integrations should be provided by deployment config. For example,
a host application can expose a Git storage proxy by adding a capability whose target is
its own internal auth-minting endpoint; the runtime service only forwards the request and
injects authoritative project identity headers.

## Backups

Backups are recovery points stored in S3-compatible object storage. Backup creation is
enabled only when object storage config is complete. `PROJECT_RUNTIME_BACKUP_ROOT`, which
defaults to `/srv/project-runtime/.project-runtime/backups` on Linux, is used only for
temporary files during backup and restore. Retention defaults to the last 5 backups per
project and can be changed with `PROJECT_RUNTIME_BACKUP_RETENTION`.

In ZFS mode, backups are compressed `zfs send` streams with `.zfs.gz` names. Restore receives
the stream into a temporary dataset and only swaps it into the project dataset after receive
succeeds, so a failed restore cannot replace a valid project with an empty filesystem.

In directory mode, backups remain `.tar.gz` archives. Restore extracts into a temporary
directory first and only moves the current project aside after extraction succeeds.

## Project archival

Project archival is separate from backups. It moves the current project filesystem out of
local hot storage after the container is stopped:

```text
local -> archiving -> archived -> restoring -> local
                    -> error
```

Use `POST /v1/projects/:id/archive` to upload the current project filesystem to object
storage, verify the upload metadata, and then remove the local project directory. Use
`POST /v1/projects/:id/unarchive` to restore it. Hot-path project operations such as
`exec`, `fs/*`, `clone`, and backup creation automatically unarchive before continuing.
If an archive restore fails, the project enters `error` and the service does not create
a fresh empty project.

Archives require complete S3-compatible object storage config. When that config is incomplete,
cold storage is disabled. In ZFS mode archives use the same compressed `zfs send` format as
backups; in directory mode they use `.tar.gz`. Retention defaults to the last 2 archive
generations per project and can be changed with `PROJECT_RUNTIME_ARCHIVE_RETENTION`.

Set `PROJECT_RUNTIME_ARCHIVE_AFTER_SECS` to a positive value to enable the background
inactivity sweeper. The sweep interval defaults to 300 seconds and can be changed with
`PROJECT_RUNTIME_ARCHIVE_SWEEP_SECS`.

S3-compatible object storage config:

```text
PROJECT_RUNTIME_OBJECT_BUCKET=...
PROJECT_RUNTIME_OBJECT_PREFIX=project-runtime
PROJECT_RUNTIME_OBJECT_ENDPOINT=https://<account>.r2.cloudflarestorage.com
PROJECT_RUNTIME_OBJECT_REGION=auto
PROJECT_RUNTIME_OBJECT_ACCESS_KEY_ID=...
PROJECT_RUNTIME_OBJECT_SECRET_ACCESS_KEY=...
PROJECT_RUNTIME_OBJECT_PATH_STYLE=true
```

## Disk headroom

`/v1/host/stats` reports total/free bytes and whether the host is above the configured reserve.
`PROJECT_RUNTIME_DISK_RESERVE_BYTES` defaults to 20 GiB. Project file mutations, clone,
and backup creation return HTTP 507 when the host drops below that reserve.

## License

MIT
