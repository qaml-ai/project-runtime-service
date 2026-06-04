# Architecture

Project Runtime Service is intended to be a backend for agent-driven project compute.

The agent product should keep product state:

- users, orgs, workspaces, billing, and auth
- project names and clone relationships
- tool schemas and agent prompts
- credential references and policy

The runtime service should keep runtime state:

- project disks
- container lifecycle
- file operations
- command execution
- quotas
- disk growth
- backups
- generic outbound capabilities

## Runtime backend contract

The host exposes project-oriented APIs while keeping legacy workspace routes during migration:

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
DELETE /v1/projects/:id
GET  /v1/projects/:id/backups
POST /v1/projects/:id/backups
POST /v1/projects/:id/restore
GET  /v1/projects/:id/proxies
ANY  /p/:capability/*
```

## Runtime image contract

The service should not depend on a product-specific image. The image contract is:

- workspace bind mount path is configurable
- exec user, home, and default working directory are configurable
- host-side file writes can chown to a configurable UID/GID
- `GET /health` on container port `8080` is recommended but not part of the project API

The basic default image is intentionally small and product-neutral. Product images can add
agent skills, browser tooling, language runtimes, deployment helpers, or package caches as
needed. The runtime service should continue to treat those as image choices, not protocol
features.

## Fast clones

On Linux hosts, the preferred production storage backend is ZFS.

With `PROJECT_RUNTIME_STORAGE_DRIVER=zfs`, every project is a ZFS dataset mounted at the
normal project path. Fast clone should:

1. Lock source and target.
2. Ensure the source project dataset exists.
3. Reject if the target dataset or target path exists.
4. Stop the source container briefly.
5. Snapshot the source dataset.
6. Run `zfs clone` into the target dataset with the target mountpoint.
7. Apply the target `refquota`.
8. Start the target container lazily on first use.

The clone-origin snapshot is live metadata, because ZFS clones depend on it. It should not be
deleted as routine backup cleanup.

Directory mode remains available for simple hosts. In that mode, fast clone requires XFS with
reflinks enabled and copies with `cp -a --reflink=always`. Do not silently fall back to a full
copy in the fast clone API. If fast cloning is unavailable, return a capability error so the
caller can choose a slower explicit migration path.

## Generic outbound capabilities

The proxy design follows the same broad pattern as exe.dev integrations:

- create a named capability
- attach it to a project, project label, or default policy
- expose it as an internal hostname or local proxy URL inside the container
- inject credentials outside the container
- set authoritative caller headers after stripping spoofable input headers

The container should never receive the upstream credential. It receives only the ability to use
an attached capability.

Capabilities are loaded from a JSON file. The default Linux path is
`/etc/project-runtime-service/proxies.json`; `PROJECT_RUNTIME_PROXY_CAPABILITIES_FILE`
can point at a different file.

Example:

```json
{
  "capabilities": [
    {
      "name": "github-repo",
      "target": "https://github.com/qaml-ai/example",
      "bearerToken": "host-held-token",
      "allowedProjects": ["project-example", "example"]
    }
  ]
}
```

Useful auth plugins:

- `none`
- `bearer`
- `basic`
- `static_header`
- `aws_sigv4`
- `oauth_bearer`
- `peer`

## Service authentication

The first production version can use a scoped bearer token between the agent app and runtime service.
For multi-host or customer-managed deployments, support mTLS as a stronger option.

The auth interface should be configurable:

```bash
CONTROL_PLANE_AUTH_TYPE=bearer
CONTROL_PLANE_BEARER_TOKEN=...
```

or:

```bash
CONTROL_PLANE_AUTH_TYPE=mtls
CONTROL_PLANE_TLS_CERT_FILE=/etc/project-runtime/server.crt
CONTROL_PLANE_TLS_KEY_FILE=/etc/project-runtime/server.key
CONTROL_PLANE_TLS_CLIENT_CA_FILE=/etc/project-runtime/client-ca.crt
```

Project containers should not receive deploy tokens. For app API proxying, the runtime
service authenticates to the app Worker with either mTLS or `PROJECT_RUNTIME_PROXY_SECRET`
and injects authoritative `X-Chiridion-*` project identity headers.
Deploy tooling should use the static docker-facing endpoint
`http://host.docker.internal:8081/deploy/client/v4`; the project identity comes from
the caller container's host-side metadata, not from the URL.

## Data-loss guardrails

Backups are written to S3-compatible object storage with retention controlled by
`PROJECT_RUNTIME_BACKUP_RETENTION`.

In ZFS mode, backups are compressed `zfs send` streams. Restore receives into a temporary
dataset and only renames it into place after the stream has been fully received. If receive
fails, the existing project remains in place and the project does not start with an
accidentally empty filesystem.

In directory mode, backups are tar.gz archives. Restore extracts into a temporary directory
and only swaps the current project out after the archive has been fully read.

The host also enforces a configurable free-space reserve. `PROJECT_RUNTIME_DISK_RESERVE_BYTES`
defaults to 20 GiB, `/v1/host/stats` reports current headroom, and project creation/mutations,
clone, and backup creation fail with HTTP 507 below the reserve.

## Remaining future work

- provider-specific disk auto-grow
- richer capability auth plugins beyond static bearer/header injection
- product-neutral setup script and systemd unit names
