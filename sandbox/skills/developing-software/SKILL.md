---
name: developing-software
description: Deploy software to the internet using Cloudflare Workers. Use this skill when the user asks to deploy APIs, web apps, fullstack applications, or AI-powered apps. Handles Workers, Durable Objects with SQLite storage, real-time WebSocket connections, and AI chat agents with the Vercel AI SDK.
license: Complete terms in LICENSE.txt
---

# Deploying Software to Cloudflare

This skill guides deployment of production software to Cloudflare's edge network using Workers and Durable Objects.

## Core Principles

1. **Use `create-worker` to scaffold projects** - Do not use `wrangler init` or `npm create cloudflare`
2. **Deploy Cloudflare Workers** - The infrastructure is already configured for Worker deployments
3. **Use Durable Objects with SQLite backends** - This is the primary persistence mechanism
4. **Use React Router 7 framework mode for fullstack web apps** - It is the successor to Remix; default to route `loader()`/`action()` patterns, not SPA-style client data fetching
5. **Use shadcn/ui for frontend components** - Use `bunx --bun shadcn@latest add <component>` to add components

## Using Agent Teams for Parallel Development

When building applications with multiple independent components that can be developed in parallel, consider using an **agent team** approach to speed up development. This is especially effective for:

- **Separate frontend and backend work** - One agent builds the API routes while another creates the UI components
- **Multiple independent features** - Different agents implement different features simultaneously (e.g., authentication, data dashboard, admin panel)
- **Multi-page applications** - Each agent owns different routes or pages
- **Component library + integration** - One agent builds reusable components while another integrates them into pages

### When to Use Agent Teams

Use agent teams when work can be cleanly separated into independent pieces:

- ✅ Frontend UI and backend API development
- ✅ Multiple route handlers with different responsibilities
- ✅ Separate Durable Object classes for different domains
- ✅ Independent shadcn/ui component implementations
- ✅ Database schema + application logic that uses it

Avoid agent teams when:

- ❌ Work has tight coupling and constant back-and-forth
- ❌ The task is small and wouldn't benefit from parallelization
- ❌ Clear boundaries between components haven't been established

### How to Structure Agent Team Work

1. **Define clear boundaries** - Decide which agent owns which files/features
2. **Establish interfaces first** - Agree on API contracts, types, and schemas before parallel work
3. **Minimize shared files** - Each agent should work on separate files when possible
4. **Coordinate integration** - Have one agent handle final integration and testing

Example workflow for a data dashboard app:
- **Agent 1**: Build Durable Object with SQLite schema and data access methods
- **Agent 2**: Create shadcn/ui components for charts, tables, and forms
- **Agent 3**: Implement React Router routes that connect the UI to the backend

This parallel approach can significantly reduce total development time for complex applications.

## Creating New Projects

Use the `create-worker` command to scaffold new projects. Do NOT use `wrangler init` or `npm create cloudflare`.

```bash
# Create a fullstack React app with defaults
create-worker my-app

# Publish any file (notebook, markdown, CSV, etc.) as a standalone app
publish my-notebook --file ./analysis.ipynb

# Customize the UI style and theme
create-worker my-app --style nova --theme blue

# Full customization example
create-worker my-app --style lyra --theme emerald --font figtree --radius large

# See all options
create-worker --help
```

### Style Options

| Option | Values | Default | Description |
|--------|--------|---------|-------------|
| `--style` | vega, nova, maia, lyra, mira | mira | UI style preset |
| `--theme` | neutral, amber, blue, cyan, emerald, fuchsia, green, indigo, lime, orange, pink, purple, red, rose, sky, teal, violet, yellow, zinc, gray, stone | neutral | Theme color |
| `--base-color` | neutral, zinc, gray, stone | neutral | Base gray color (must match theme if theme is zinc/gray/stone) |
| `--font` | inter, noto-sans, nunito-sans, figtree | inter | Font family |
| `--radius` | default, none, small, medium, large | default | Border radius |
| `--menu-color` | default, inverted | default | Menu color style |
| `--menu-accent` | subtle, bold | subtle | Menu accent style |

## Deployment Commands

```bash
# Deploy to production
bun run deploy

# View logs
bun wrangler tail
```

> **Note:** `wrangler dev` is not available. Deployments are fast - just deploy and iterate in the cloud.

## Post-Deployment Verification

After deploying a worker, use MCP tools to verify the deployment and get the live URL:

1. **Get the deployed app URL** - Use the `list_apps` MCP tool to retrieve the URL of deployed workers
2. **Take a screenshot** - Use the `take_screenshot` MCP tool (local Playwright) with the full URL to capture the deployed app and verify it looks correct

```bash
# Example workflow after bun run deploy
# 1. List apps to get the URL
#    → Use MCP tool: list_apps

# 2. Take a screenshot to verify the UI
#    → Use MCP tool: take_screenshot with the app URL from step 1
```

The `take_screenshot` tool runs Playwright locally inside the container for fast, reliable screenshots. It accepts a full URL and optional viewport dimensions (`width`, `height`) and `wait_for_timeout` (extra ms to wait after page load).

This ensures the deployment succeeded and the app renders correctly before sharing the URL with the user.

## Durable Objects with SQLite Storage

SQLite-backed Durable Objects are the recommended persistence layer. Each Durable Object instance has its own private SQLite database with up to 10GB of storage.

### Configuration

In `wrangler.jsonc`, use `new_sqlite_classes` for SQLite-backed DOs:

```jsonc
{
  "durable_objects": {
    "bindings": [
      {
        "name": "MY_DO",
        "class_name": "MyDurableObject"
      }
    ]
  },
  "migrations": [
    {
      "tag": "v1",
      "new_sqlite_classes": ["MyDurableObject"]
    }
  ]
}
```

### SQLite Storage API

Access SQLite via `this.ctx.storage.sql`:

```typescript
import { DurableObject } from "cloudflare:workers";

export class MyDurableObject extends DurableObject<Env> {
  sql = this.ctx.storage.sql;

  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    // Initialize schema
    this.sql.exec(`
      CREATE TABLE IF NOT EXISTS items (
        id TEXT PRIMARY KEY,
        data TEXT NOT NULL,
        created_at INTEGER DEFAULT (unixepoch())
      )
    `);
  }

  async getItem(id: string) {
    const result = this.sql.exec(
      "SELECT * FROM items WHERE id = ?",
      id
    ).one();
    return result;
  }

  async setItem(id: string, data: string) {
    this.sql.exec(
      "INSERT OR REPLACE INTO items (id, data) VALUES (?, ?)",
      id,
      data
    );
  }

  async listItems() {
    return this.sql.exec("SELECT * FROM items ORDER BY created_at DESC").toArray();
  }
}
```

### Key SQLite Methods

| Method | Description |
|--------|-------------|
| `sql.exec(query, ...params)` | Execute a SQL statement with optional parameters |
| `.one()` | Return a single row (or null) |
| `.toArray()` | Return all rows as an array |
| `.raw()` | Return raw column arrays instead of objects |

### Transactions

Use `this.ctx.storage.transactionSync()` for atomic operations:

```typescript
this.ctx.storage.transactionSync(() => {
  this.sql.exec("UPDATE accounts SET balance = balance - ? WHERE id = ?", amount, fromId);
  this.sql.exec("UPDATE accounts SET balance = balance + ? WHERE id = ?", amount, toId);
});
```

### Point-in-Time Recovery (PITR)

SQLite-backed DOs support restoring to any point in the past 30 days:

```typescript
// Get a bookmark for current state
const bookmark = await this.ctx.storage.getCurrentBookmark();

// Restore to a previous point
await this.ctx.storage.restoreFromBookmark(previousBookmark);
```

## KV Storage APIs

In addition to SQLite, Durable Objects provide key-value storage APIs. There are two variants:

### Synchronous KV Storage (Recommended)

SQLite-backed Durable Objects have access to a fast, synchronous KV API via `ctx.storage.kv`. This is the preferred KV storage method when using SQLite-backed DOs.

```typescript
export class MyDurableObject extends DurableObject<Env> {
  async handleRequest() {
    // Synchronous operations - no await needed
    this.ctx.storage.kv.put("key", { foo: "bar" });
    const value = this.ctx.storage.kv.get("key");

    // Delete a key
    this.ctx.storage.kv.delete("key");

    // Check if key exists
    const exists = this.ctx.storage.kv.has("key");

    // List keys with optional prefix
    const keys = this.ctx.storage.kv.list(); // returns Map
    const prefixed = this.ctx.storage.kv.list({ prefix: "user:" });

    // Batch operations
    this.ctx.storage.kv.put(new Map([
      ["key1", "value1"],
      ["key2", "value2"]
    ]));

    // Get multiple keys
    const values = this.ctx.storage.kv.get(["key1", "key2"]); // returns Map
  }
}
```

| Method | Description |
|--------|-------------|
| `kv.get(key)` | Get a single value |
| `kv.get(keys[])` | Get multiple values (returns Map) |
| `kv.put(key, value)` | Store a single value |
| `kv.put(entries)` | Store multiple values (accepts Map or entries) |
| `kv.delete(key)` | Delete a single key |
| `kv.delete(keys[])` | Delete multiple keys |
| `kv.has(key)` | Check if key exists |
| `kv.list(options?)` | List keys with optional prefix/limit |

### Legacy Async KV Storage

The original async KV storage API is still available and works with both SQLite-backed and legacy KV-backed Durable Objects:

```typescript
export class MyDurableObject extends DurableObject<Env> {
  async handleRequest() {
    // Async operations - requires await
    await this.ctx.storage.put("key", { foo: "bar" });
    const value = await this.ctx.storage.get("key");

    // Delete
    await this.ctx.storage.delete("key");

    // List keys
    const entries = await this.ctx.storage.list({ prefix: "user:" });

    // Batch operations
    await this.ctx.storage.put({
      key1: "value1",
      key2: "value2"
    });

    // Get multiple
    const values = await this.ctx.storage.get(["key1", "key2"]);
  }
}
```

### When to Use KV vs SQLite

| Use Case | Recommendation |
|----------|----------------|
| Simple key-value data | Sync KV (`ctx.storage.kv`) |
| Relational data with queries | SQLite (`ctx.storage.sql`) |
| Full-text search, joins, aggregations | SQLite |
| Session/config data | Sync KV |
| High-frequency reads of single keys | Sync KV |
| Complex transactions | SQLite |

## Hibernatable WebSockets

Durable Objects support WebSocket connections that can hibernate to save costs. During hibernation, the DO is evicted from memory but connections remain open.

### Why Use Hibernatable WebSockets

- **Cost savings** - No duration charges during idle periods
- **Persistent connections** - Clients stay connected even when DO hibernates
- **Automatic wake** - DO recreates when a message arrives

### Implementation

```typescript
import { DurableObject } from "cloudflare:workers";

export class WebSocketDO extends DurableObject<Env> {
  async fetch(request: Request): Promise<Response> {
    const webSocketPair = new WebSocketPair();
    const [client, server] = Object.values(webSocketPair);

    // Accept with hibernation support
    this.ctx.acceptWebSocket(server);

    return new Response(null, {
      status: 101,
      webSocket: client,
    });
  }

  // Called when a message arrives (even after hibernation)
  async webSocketMessage(ws: WebSocket, message: string | ArrayBuffer) {
    const data = typeof message === "string" ? message : new TextDecoder().decode(message);

    // Broadcast to all connected clients
    for (const socket of this.ctx.getWebSockets()) {
      socket.send(data);
    }
  }

  // Called when connection closes
  async webSocketClose(ws: WebSocket, code: number, reason: string, wasClean: boolean) {
    ws.close(code, reason);
  }

  // Called on connection error
  async webSocketError(ws: WebSocket, error: unknown) {
    console.error("WebSocket error:", error);
    ws.close(1011, "Unexpected error");
  }
}
```

### Key Hibernation APIs

| Method | Description |
|--------|-------------|
| `ctx.acceptWebSocket(ws, tags?)` | Accept WebSocket with hibernation support |
| `ctx.getWebSockets(tag?)` | Get all connected WebSockets (optionally filtered by tag) |
| `ws.serializeAttachment(value)` | Store up to 2KB of state per connection |
| `ws.deserializeAttachment()` | Retrieve stored connection state |
| `ctx.setWebSocketAutoResponse(request, response)` | Auto-respond to pings without waking DO |

### Auto-Response for Ping/Pong

Avoid waking the DO for heartbeat messages:

```typescript
constructor(ctx: DurableObjectState, env: Env) {
  super(ctx, env);
  // Automatically respond to "ping" with "pong" without waking
  ctx.setWebSocketAutoResponse(
    new WebSocketRequestResponsePair("ping", "pong")
  );
}
```

### Persisting State Across Hibernation

Store per-connection metadata that survives hibernation:

```typescript
async fetch(request: Request): Promise<Response> {
  const url = new URL(request.url);
  const userId = url.searchParams.get("userId");

  const webSocketPair = new WebSocketPair();
  const [client, server] = Object.values(webSocketPair);

  this.ctx.acceptWebSocket(server);

  // Store user info (survives hibernation, max 2KB)
  server.serializeAttachment({ userId, connectedAt: Date.now() });

  return new Response(null, { status: 101, webSocket: client });
}

async webSocketMessage(ws: WebSocket, message: string | ArrayBuffer) {
  // Retrieve stored state after hibernation wake
  const attachment = ws.deserializeAttachment();
  console.log(`Message from user ${attachment.userId}`);
}
```

### Tagging WebSockets

Organize connections by tags for targeted messaging:

```typescript
// Accept with tags
this.ctx.acceptWebSocket(server, ["room:123", "user:456"]);

// Get all sockets in a room
const roomSockets = this.ctx.getWebSockets("room:123");
for (const socket of roomSockets) {
  socket.send(JSON.stringify({ type: "room-message", data }));
}
```

## Fullstack Apps with React + Vite

For fullstack applications, use the `create-worker` command:

```bash
# Create React app with Vite
create-worker my-app

# Or with custom styling
create-worker my-app --style nova --theme blue

cd my-app

# Install dependencies
bun install

# Add shadcn/ui components
bunx --bun shadcn@latest add button card form input

# Local development
bun dev

# Deploy
bun run deploy
```

The template includes:
- React 19 with React Router 7
- Vite for fast builds and HMR
- Tailwind CSS v4
- shadcn/ui pre-configured
- TypeScript
- Cloudflare Worker entry in `workers/app.ts`
- Server data/mutation patterns via route `loader()` and `action()`
- wrangler as a local dependency
- `@cloudflare/codemode` for LLM tool orchestration (active by default)
- `worker_loaders` binding for codemode runtime

### Wrangler Configuration for React + Vite

The template creates this `wrangler.jsonc`:

```jsonc
{
  "name": "my-app",
  "main": "./workers/app.ts",
  "compatibility_date": "2024-12-01",
  "compatibility_flags": ["nodejs_compat"],
  "assets": {
    "directory": "./public/",
    "binding": "ASSETS"
  },
  "worker_loaders": [{ "binding": "LOADER" }],
  "services": [
    { "binding": "DATA_PROXY", "service": "my-app", "entrypoint": "LocalDataProxyService" },
    { "binding": "CONNECTIONS", "service": "my-app", "entrypoint": "LocalConnectionsService" },
    { "binding": "CAMELAI", "service": "my-app", "entrypoint": "LocalCamelAiService" }
  ]
}
```

### Adding API Routes

For JSON API endpoints, add a React Router route under `app/routes/` and wire it in `app/routes.ts`.
Use route `loader()` for GET handlers and `action()` for POST/PUT/DELETE handlers.

```typescript
// app/routes/api.items.ts
import type { Route } from "./+types/api.items";
import { data } from "react-router";

export async function loader({ context }: Route.LoaderArgs) {
  const id = context.cloudflare.env.EXAMPLE_DO.idFromName("global");
  const stub = context.cloudflare.env.EXAMPLE_DO.get(id);
  const items = await stub.listItems();

  return data({ items });
}

export async function action({ request, context }: Route.ActionArgs) {
  const payload = await request.json();

  const id = context.cloudflare.env.EXAMPLE_DO.idFromName("global");
  const stub = context.cloudflare.env.EXAMPLE_DO.get(id);

  await stub.createItem(payload);
  return data({ ok: true }, { status: 201 });
}

// app/routes.ts
import { route, type RouteConfig } from "@react-router/dev/routes";

export default [
  route("api/items", "routes/api.items.ts"),
] satisfies RouteConfig;
```

### React Router 7: Non-Navigational Mutations

In Framework Mode with SSR, loaders are automatically revalidated after navigations and form submissions. Revalidation is not a full browser page reload, but it can feel page-wide because loader data is fetched again and the route tree rerenders.

Use these rules:

- If the submit should feel like navigation, use normal `<Form>`.
- If the submit should feel inline/local, use:
  - `<Form navigate={false} fetcherKey="some-key">` if you want to keep the `<Form>` API
  - `useFetcher({ key: "some-key" })` to read the submission state/result for that keyed form
  - or `<fetcher.Form>` if you want the fetcher object directly

Important:

- `reloadDocument` causes a real full page reload. Revalidation does not.
- `navigate={false}` prevents navigational form behavior by using a fetcher internally.
- Even with `navigate={false}`, Framework Mode may still revalidate loaders after the action, which can make the UI feel like it reloaded.
- If the mutation should feel truly local, consider:
  - returning JSON from `action()` instead of redirecting
  - rendering from `fetcher.data`
  - using `fetcher.formData` for optimistic UI
  - exporting `shouldRevalidate()` to skip unnecessary loader revalidation for successful local mutations

Recommended pattern for inline toggles, deletes, likes, and similar actions:

1. Use `<Form method="post" navigate={false} fetcherKey="item-actions">`
2. Read the shared fetcher with `useFetcher({ key: "item-actions" })`
3. Return structured JSON from `action()`
4. Use optimistic UI from `fetcher.formData`
5. Use `shouldRevalidate()` only when you want to suppress route-wide revalidation
6. Reserve redirects for submissions that are actually navigational

### Organizing API Routes

For larger APIs, split endpoints by resource:

```typescript
// app/routes.ts
export default [
  route("api/items", "routes/api.items.ts"),
  route("api/items/:id", "routes/api.items.$id.ts"),
] satisfies RouteConfig;
```

## AI-Powered Apps

The template includes pre-configured AI chat capabilities using the Vercel AI SDK with Cloudflare Workers AI via the platform-virtualized `AI` binding. **The code is commented out by default**—just uncomment to enable:

1. In `wrangler.jsonc`: Uncomment the `Chat` DO binding and migration
2. In `wrangler.jsonc`: Add the native AI binding: `"ai": { "binding": "AI" }`
3. In `workers/app.ts`: Uncomment the `Chat` export and `routeAgentRequest` call
4. In `app/routes.ts`: Add the chat route

Features include:
- Automatic conversation history persistence
- Resumable streaming via WebSockets
- Tool use and function calling

> **Critical:** Every `useAgent` call MUST pass a unique `name` (e.g., a session ID) — without it, all users share one DO instance. Generate session IDs server-side in loaders, not in component bodies. Also, `useAgentChat` does NOT return `input`/`setInput`/`handleSubmit` in AI SDK v3 — manage your own input state with `useState` and use `sendMessage` to send.

**See [AI-APPS.md](AI-APPS.md) for setup, session isolation, and common pitfalls.**

### camelAI AI Access Patterns

- **In deployed workers:** Prefer `env.AI` with `workers-ai-provider` (`createWorkersAI({ binding: env.AI })`).
- **Model routing:** The platform virtualizes the AI binding with platform-controlled model selection.
- **Token caps:** Avoid setting `max_tokens` unless the user explicitly needs a hard output cap. Thinking/reasoning tokens count toward that budget and can cut answers off early. If required, set a generous cap and call out truncation risk.

## R2 Object Storage

User workers can use R2 bucket bindings for storing files, images, blobs, and any unstructured data. Buckets are virtualized — you can use any bucket name and don't need to create them ahead of time. Just declare the binding and start using it.

### Configuration

Add `r2_buckets` to `wrangler.jsonc`:

```jsonc
{
  "r2_buckets": [
    { "binding": "MY_BUCKET", "bucket_name": "myapp-uploads" }
  ]
}
```

You can use any `bucket_name` — buckets are created automatically on first use. Use project-specific names (e.g. `myapp-uploads` instead of just `uploads`) to avoid collisions with other projects in the same workspace. You can declare multiple buckets:

```jsonc
{
  "r2_buckets": [
    { "binding": "UPLOADS", "bucket_name": "myapp-uploads" },
    { "binding": "CACHE", "bucket_name": "myapp-cache" }
  ]
}
```

### Usage

Access R2 bindings through `env` just like any other Cloudflare binding:

```typescript
// Store an object
await env.MY_BUCKET.put('images/photo.jpg', imageData, {
  httpMetadata: { contentType: 'image/jpeg' }
});

// Retrieve an object
const obj = await env.MY_BUCKET.get('images/photo.jpg');
if (obj) {
  const data = await obj.text(); // or .arrayBuffer(), .json(), .blob()
}

// List objects
const listed = await env.MY_BUCKET.list({ prefix: 'images/' });
for (const obj of listed.objects) {
  console.log(obj.key, obj.size);
}

// Delete an object
await env.MY_BUCKET.delete('images/photo.jpg');
```

### In Route Loaders/Actions

```typescript
export async function action({ request, context }: Route.ActionArgs) {
  const formData = await request.formData();
  const file = formData.get('file') as File;

  await context.cloudflare.env.UPLOADS.put(
    `files/${file.name}`,
    file.stream(),
    { httpMetadata: { contentType: file.type } }
  );

  return { success: true };
}

export async function loader({ context, params }: Route.LoaderArgs) {
  const obj = await context.cloudflare.env.UPLOADS.get(`files/${params.filename}`);
  if (!obj) return new Response('Not found', { status: 404 });

  return new Response(obj.body, {
    headers: { 'Content-Type': obj.httpMetadata?.contentType || 'application/octet-stream' }
  });
}
```

### Key R2 Methods

| Method | Description |
|--------|-------------|
| `put(key, value, options?)` | Store an object (string, ArrayBuffer, ReadableStream, Blob) |
| `get(key)` | Retrieve an object with body |
| `head(key)` | Get object metadata without body |
| `delete(key)` | Delete one or more objects |
| `list(options?)` | List objects with optional prefix, limit, cursor |
| `createMultipartUpload(key)` | Start a multipart upload for large files |

### When to Use R2 vs SQLite

| Use Case | Recommendation |
|----------|----------------|
| File/image/blob storage | R2 |
| Structured/relational data | SQLite (Durable Objects) |
| JSON documents with queries | SQLite |
| Large binary assets | R2 |
| User uploads | R2 |
| Session/config data | SQLite KV |

## Connections Binding

User workers can call workspace connections through the platform-virtualized `CONNECTIONS` service binding. Use it when an app needs to call a connected provider such as Stripe, GitHub, Linear, or Notion without putting user credentials in Worker code or env vars.

The starter template includes a local `CONNECTIONS` self-binding:

```jsonc
{
  "services": [
    { "binding": "CONNECTIONS", "service": "my-app", "entrypoint": "LocalConnectionsService" }
  ]
}
```

On camelAI deploys, the platform rewrites this binding to the internal `ConnectionsService` with workspace/org isolation. In local dev, the template shim forwards to `CAMELAI_CONNECTIONS_URL` when that variable is available.

### Runtime API

Use `CONNECTIONS.find()` for the shortest path to one connection, or `CONNECTIONS.methods()` to inspect all available connection aliases, method names, input schemas, and copyable examples. Use `createConnections()` from the starter template for method-style calls:

```typescript
import { createConnections } from "~/lib/connections";

export async function action({ context }: Route.ActionArgs) {
  const stripe = await context.cloudflare.env.CONNECTIONS.find("stripe");
  const connections = createConnections(context.cloudflare.env);

  const customer = await connections[stripe.alias].createCustomer({
    email: "customer@example.com",
  });

  return { customer };
}
```

Available methods:

| Method | Description |
|--------|-------------|
| `list()` | List workspace connections available to the Worker |
| `get(connection)` | Resolve one connection by id, name, or type |
| `tools(connection)` | List MCP-backed tools for a connection |
| `methods()` | List available connection aliases and method schemas |
| `find(query)` | Resolve one connection method catalog entry by alias, id, type, name, or `{ type }`; throws on missing or ambiguous matches |
| `test(query)` | Run a quick smoke test; database-style connections run `SELECT 1 AS ok` |

Prefer connection ids or aliases when a workspace may have multiple connections of the same type. Name/type lookup is convenient, but ambiguous matches throw and ask for an id or alias.

When the connection or method name comes from user input, validate it against `CONNECTIONS.methods()` before calling the method facade:

```typescript
import { createConnections } from "~/lib/connections";

export async function action({ context, request }: Route.ActionArgs) {
  const form = await request.formData();
  const connection = String(form.get("connection") ?? "stripe");
  const method = String(form.get("method") ?? "listCustomers");
  const methods = await context.cloudflare.env.CONNECTIONS.methods();

  if (!methods.some((entry) =>
    entry.alias === connection &&
    entry.methods.some((candidate) => candidate.name === method)
  )) {
    throw new Response("Unknown connection method", { status: 400 });
  }

  const connections = createConnections(context.cloudflare.env);
  const result = await connections[connection][method]({ limit: 10 });

  return { result };
}
```

When testing connection calls in the Pi agent harness, use the `js_exec` tool. This is the preferred way for the agent to call workspace connections.

Inside `js_exec`, these globals are available:
- `env.CONNECTIONS` - method-style facade for inspecting and calling connection tools.
- `context.cloudflare.env.CONNECTIONS` - the same method-style facade.
- `connections` and `context.cloudflare.connections` - aliases for the same method-style facade.
- `env.AI` and `context.cloudflare.env.AI` - the virtual AI binding (`run()` only), matching deployed user workers.
- `env.CAMELAI` and `context.cloudflare.env.CAMELAI` - image generation service binding (`generateImage(prompt)`), same pattern as `CONNECTIONS`.

Connection credentials are intentionally hidden behind the virtual binding.

Prefer `find()` and normalized methods for common workflows:

```javascript
const clickhouse = await env.CONNECTIONS.find("clickhouse");
const result = await env.CONNECTIONS[clickhouse.alias].query({
  query: "SELECT 1 AS ok",
});

return result;
```

Use the full method catalog when you need to inspect schemas or examples:

```javascript
const catalog = await env.CONNECTIONS.methods();
return catalog.map((entry) => ({
  alias: entry.alias,
  type: entry.connection.type,
  examples: entry.methods.map((method) => method.example),
}));
```

Global facade access also works:

```javascript
return await env.CONNECTIONS.stripeProd.listCustomers({ limit: 10 });
```

Custom connections with type `other` expose a generic authenticated HTTP method named
`fetch`. Use it like normal `fetch(input, init)` instead of looking for API keys
in environment variables:

```javascript
const custom = await env.CONNECTIONS.find({ type: "other" });

const response = await env.CONNECTIONS[custom.alias].fetch("/v1/items?limit=10", {
  method: "GET",
});

return await response.json();
```

`fetch` resolves relative URLs against the connection's configured `base_url` and
camelAI applies the stored auth settings automatically. The returned value is a
standard `Response`, so use `response.ok`, `await response.text()`, and
`await response.json()` as usual.

The `js_exec` runtime also exposes every registered harness tool on the global `tools` object. Tool names, descriptions, and parameter schemas are available in `ALL_TOOLS`. Use this when code-mode JavaScript needs web lookup, workspace file/shell operations, scheduled prompts, app/domain tools, user prompts, subagents, or any other harness tool:

```javascript
const available = ALL_TOOLS.map((tool) => ({ name: tool.name, parameters: tool.parameters }));
const search = await tools.WebSearch({ query: "Cloudflare Workers Durable Objects", numResults: 3 });
const page = await tools.WebFetch({ url: search.results[0].url });
return { available, search, page };
```

For web search and page retrieval, prefer `tools.WebSearch(...)` and `tools.WebFetch(...)` because they use the harness tooling and format results consistently. Global `fetch()` is also available in `js_exec` for direct HTTP calls to public APIs and other endpoints.

Test hosted AI calls from `js_exec` with the same binding shape as deployed workers:

```javascript
const result = await env.AI.run("auto", {
  messages: [{ role: "user", content: "Say hello in one sentence." }],
});
return result;
```

Generate images the same way as in deployed workers:

```javascript
const { text, imageDataUrl } = await env.CAMELAI.generateImage(
  "Flat vector robot mascot on a solid bright green background",
);
return { text, imageDataUrl };
```

## Virtual AI Binding

User workers can use a Cloudflare-style AI binding. Add this to `wrangler.jsonc`:

```jsonc
{
  "ai": { "binding": "AI" }
}
```

Use it with the Workers AI provider:

```typescript
import { createWorkersAI } from "workers-ai-provider";

const workersai = createWorkersAI({ binding: context.cloudflare.env.AI });

const result = await generateText({
  model: workersai("auto", {}),
  prompt: "Summarize this document.",
});
```

On camelAI deploys, the platform virtualizes this binding with platform-controlled model routing.

### Model Tiers

Four tiers are available via `workersai(tier, {})` (or `env.AI.run(tier, ...)`) in deployed workers. The platform resolves each tier to a concrete model based on the org's configured AI provider (Anthropic / OpenAI / Bedrock / OpenRouter); orgs without a configured key fall back to OpenRouter via camelAI-managed credits.

| Tier | Purpose | When to Use |
|------|---------|-------------|
| `cheap` | Cheapest small model | High-volume, low-stakes work — classification, simple extraction, short replies |
| `fast` | Same low-latency model as `cheap` | When latency matters more than reasoning depth |
| `auto` | Balanced default | General-purpose AI features — the right pick when you're unsure |
| `smart` | Strongest reasoning model | Complex reasoning, long-context analysis, agentic tool use |

Default to `auto`. Any OpenRouter model id (e.g. `anthropic/claude-sonnet-4.6`) is also accepted as a pass-through; `:nitro` is appended automatically to route via the fastest provider. For images, use `env.CAMELAI.generateImage(prompt)` (requires `CAMELAI` service binding in `wrangler.jsonc`). Returns `{ text, imageDataUrl, images }`. The virtual `AI` binding only exposes `run()`.

The starter template includes a local `CAMELAI` self-binding (typed via `bun wrangler types` as `Service<typeof LocalCamelAiService>`):

```jsonc
{
  "services": [
    { "binding": "CAMELAI", "service": "my-app", "entrypoint": "LocalCamelAiService" }
  ]
}
```

On camelAI deploys, the platform rewrites this binding to the internal `CamelAiService` entrypoint (same pattern as `CONNECTIONS`).

### Codemode (Tool Orchestration — Preferred for Agents with Tools)

When building AI agents that have access to tools, **prefer codemode** over plain tool calling. Codemode lets the LLM orchestrate multiple tools in a single turn by writing TypeScript code, which is faster and more capable than sequential one-tool-at-a-time calling. It requires:
- `worker_loaders` binding (`env.LOADER`) — ephemeral isolate runtime

Use `createCodeTool` + `DynamicWorkerExecutor` and add `outputSchema` to tools for typed code generation. Always set `stopWhen: stepCountIs(100)` for multi-step tool use. Use plain tool calling only for simple cases with one or two tools that don't need chaining.

**Critical codemode integration notes:**
- **Only wrap data-access tools** — do NOT create pass-through tools (e.g. `createVisualization`) that just echo inputs. The LLM constructs output shapes directly in code.
- **Define a return type convention** in the `createCodeTool` description using a `type` discriminator field (e.g., `{ type: "chart", ... }` or `{ type: "table", ... }`). Include the `{{types}}` placeholder and concrete examples.
- **Frontend rendering** — the AI SDK v5+ uses `p.type === "tool-codemode"` (NOT `"tool-invocation"`), `p.state === "output-available"` (NOT `"result"`), and data in `p.output` (NOT `p.result`). The codemode output shape is `{ code, result, logs }` — your discriminated return value is in `p.output.result`.
- **Zod defensive defaults** — tool parameters may arrive as `undefined` despite Zod schemas. Always add `const metric = params.metric ?? "count"` in execute functions (use `??` not `||` to preserve valid falsy values).

**See [AI-APPS.md](AI-APPS.md) for full codemode setup, frontend rendering patterns, and common pitfalls.**

## E2E Testing with Playwright

The starter template includes Playwright as a devDependency and scaffolded E2E smoke tests in `e2e/smoke.test.mjs` (commented out). Browser binaries are installed on demand by the project when E2E tests are needed. Uncomment the tests, update the `APP_URL`, install the browser with `bunx playwright install chromium` if needed, and run with `bun run test:e2e`. The test file includes auth cookie boilerplate for accessing private deployed apps.

## Troubleshooting

### Recharts SSR Warnings
Recharts `ResponsiveContainer` emits width/height warnings during SSR — these are **expected and benign**. Do not add `ClientOnly` wrappers or other workarounds to suppress them; the extra complexity is not worth it.

### Screenshot Tool Timeouts
If `take_screenshot` times out after deployment, consider **server-side data fetching latency** as the likely cause before blaming the rendering layer. Slow loaders or API calls delay the initial page render, which causes the screenshot to time out. Fix by adding React `<Suspense>` boundaries with skeleton fallbacks around data-dependent sections so the page shell renders immediately while data loads.

## Design Defaults

Every project should ship with polished design fundamentals. When building any app, always include these from the start:

1. **Typography as a design asset** - Treat font selection like choosing imagery — fonts carry personality and set the tone before a user reads a word. Every app needs at minimum a **display font** (for headings, heroes, and accent moments) and a **body font**. Match the display font's boldness to the project's personality: creative or editorial sites call for expressive typefaces (e.g., Danfo, Playfair Display, Fraunces), while SaaS tools and dashboards suit more refined ones (e.g., Instrument Serif, Space Grotesk, DM Sans). The display font should appear throughout the site — not just the hero — reinforcing identity across pages. Some projects benefit from a third family for UI labels or data. Choose fonts early, before layout work begins, as they shape the entire design direction. Import via Google Fonts `@import` or `<link>` in the root layout.

2. **Favicon** - Every deployed app must have a favicon. Create or generate an SVG favicon (`public/favicon.svg`) that reflects the app's purpose or brand. Reference it in the root `<head>` with `<link rel="icon" type="image/svg+xml" href="/favicon.svg">`. A simple, recognizable icon is always better than the browser default.

3. **OpenGraph meta tags and image** - Add `og:title`, `og:description`, and `og:image` meta tags in the root route or layout head so the app looks professional when shared on social media, Slack, or messaging apps. Create a `public/og-image.png` (1200x630px recommended) with the app name, a brief tagline, and relevant visuals. Also include `twitter:card=summary_large_image` and `twitter:image` meta tags.

## Best Practices

1. **One DO class per domain concept** - e.g., `UserDO`, `RoomDO`, `SessionDO`
2. **Use SQLite for relational data** - Queries, joins, and complex transactions
3. **Use sync KV for simple key-value data** - `ctx.storage.kv` is fast and synchronous
4. **Initialize schema in constructor** - Use `CREATE TABLE IF NOT EXISTS`
4. **Use hibernatable WebSockets for real-time** - Saves significant costs
5. **Tag WebSockets for routing** - Makes broadcasting to subsets efficient
6. **Use transactions for multi-step operations** - Ensures consistency
7. **Set auto-response for heartbeats** - Prevents unnecessary wake-ups
