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

The host should eventually expose a project-oriented API:

```text
GET  /v1/host/capabilities
POST /v1/projects/:id/ensure
POST /v1/projects/:id/exec
GET  /v1/projects/:id/files/*
PUT  /v1/projects/:id/files/*
POST /v1/projects/:id/clone
POST /v1/projects/:id/stop
DELETE /v1/projects/:id
POST /v1/projects/:id/proxies
ANY  /p/:capability/*
```

The current compatibility API still exposes workspace-oriented routes while extraction is in progress.

## Fast clones

On Linux hosts, the preferred storage backend is XFS with project quotas and reflinks enabled.

Fast clone should:

1. Lock source and target.
2. Ensure the source project exists.
3. Reject if the target exists.
4. Pause or stop the source container briefly.
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

Example:

```json
{
  "id": "github-repo",
  "targetBaseUrl": "https://github.com/qaml-ai/example",
  "allowedHosts": ["github.com"],
  "auth": {
    "type": "basic",
    "credentialRef": "workspace.github_app_token"
  }
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

```toml
[control_plane.auth]
type = "bearer"
token_file = "/etc/project-runtime/control-plane-token"
```

or:

```toml
[control_plane.auth]
type = "mtls"
client_cert = "/etc/project-runtime/client.crt"
client_key = "/etc/project-runtime/client.key"
ca_cert = "/etc/project-runtime/ca.crt"
```
