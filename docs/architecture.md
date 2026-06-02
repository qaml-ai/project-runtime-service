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
DELETE /v1/projects/:id
GET  /v1/projects/:id/backups
POST /v1/projects/:id/backups
POST /v1/projects/:id/restore
GET  /v1/projects/:id/proxies
ANY  /p/:capability/*
```

The current compatibility API still exposes workspace-oriented routes while extraction is in progress.

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

On Linux hosts, the preferred storage backend is XFS with project quotas and reflinks enabled.

Fast clone should:

1. Lock source and target.
2. Ensure the source project exists.
3. Reject if the target exists.
4. Stop the source container briefly.
5. Run `sync`.
6. Copy with `cp -a --reflink=always`.
7. Apply target project quota.
8. Atomically rename the temporary target directory.
9. Start the target container lazily on first use.

Do not silently fall back to a full copy in the fast clone API. If reflinks are unavailable,
return a capability error so the caller can choose a slower explicit migration path.

## Generic outbound capabilities

The proxy design follows the same broad pattern as exe.dev integrations:

- create a named capability
- attach it to a project, project label, or default policy
- expose it as an internal hostname or local proxy URL inside the sandbox
- inject credentials outside the sandbox
- set authoritative caller headers after stripping spoofable input headers

The sandbox should never receive the upstream credential. It receives only the ability to use
an attached capability.

Capabilities are loaded from `PROJECT_RUNTIME_PROXY_CAPABILITIES_JSON` or
`PROJECT_RUNTIME_PROXY_CAPABILITIES_FILE`.

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

## Data-loss guardrails

Backups are written as tar.gz archives with retention controlled by
`PROJECT_RUNTIME_BACKUP_RETENTION`. When S3-compatible object storage config is complete,
backups are stored in object storage; otherwise local backup storage under
`PROJECT_RUNTIME_BACKUP_ROOT` is used.

Restore extracts into a temporary directory and only swaps the current project out after the
archive has been fully read. If extraction fails, the existing project remains in place and the
project does not start with an accidentally empty filesystem.

The host also enforces a configurable free-space reserve. `PROJECT_RUNTIME_DISK_RESERVE_BYTES`
defaults to 20 GiB, `/v1/host/stats` reports current headroom, and project creation/mutations,
clone, and backup creation fail with HTTP 507 below the reserve.

## Remaining future work

- provider-specific disk auto-grow
- richer capability auth plugins beyond static bearer/header injection
- product-neutral setup script and systemd unit names
