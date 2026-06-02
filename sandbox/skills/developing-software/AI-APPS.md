# Building AI Apps and Agents

The starter template includes pre-configured AI chat scaffolding with the Vercel AI SDK and Cloudflare Workers AI. See `workers/chat.ts` for the Chat DO and `app/routes/chat.tsx` for the frontend — just uncomment to enable.

## Runtime Model

Use native `env.AI` in deployed workers. The platform virtualizes this binding
and routes model calls through camelAI-managed billing and spend tracking.
Container/runtime scripts do not have a host-side model proxy.

## Enable AI Chat in the Starter

1. In `wrangler.jsonc`, uncomment/add:
   - Chat Durable Object binding
   - Chat migration
   - `"ai": { "binding": "AI" }`
2. In `workers/app.ts`, uncomment `routeAgentRequest` and `export { Chat }`.
3. In `app/routes.ts`, add `route("chat", "routes/chat.tsx")`.

## Critical: Session Isolation with useAgent

> **Every `useAgent` call MUST pass a unique `name` to create per-user/per-session Durable Object instances.** Without `name`, all users share the same DO instance ("default"), seeing each other's conversations and overwriting each other's state. This is the #1 deployment bug with Cloudflare Agents.

```tsx
// BAD — all users share one DO instance (the "default" instance)
const agent = useAgent({ agent: "Chat" });

// GOOD — each session gets its own DO instance
const agent = useAgent({ agent: "Chat", name: sessionId });
```

### How to generate session IDs

**Always generate session IDs server-side in a loader** — never in the component body (causes hydration mismatches and re-render loops).

```tsx
// In your route loader (runs on the server)
export async function loader({ request }: Route.LoaderArgs) {
  const url = new URL(request.url);
  const sessionId = url.searchParams.get("session") || crypto.randomUUID();
  return { sessionId };
}

// In your component
export default function ChatPage() {
  const { sessionId } = useLoaderData<typeof loader>();
  // ...
  const agent = useAgent({ agent: "Chat", name: sessionId });
}
```

For apps where conversations should persist across page refreshes within a tab, store the session ID in `sessionStorage`:

```tsx
function getOrCreateSessionId(): string {
  const key = "chat-session-id";
  let id = sessionStorage.getItem(key);
  if (!id) {
    id = crypto.randomUUID();
    sessionStorage.setItem(key, id);
  }
  return id;
}
```

### AI SDK v3: useAgentChat API

`useAgentChat` (from `@cloudflare/ai-chat/react`) does **NOT** return `input`, `setInput`, `handleInputChange`, or `handleSubmit`. These were removed in AI SDK v3. Manage your own input state:

```tsx
// BAD — these are undefined in AI SDK v3, causes "X is not a function" errors
const { input, setInput, handleSubmit } = useAgentChat({ agent });

// GOOD — manage your own input state
const [input, setInput] = useState("");
const { messages, sendMessage, status, error, clearHistory } = useAgentChat({
  agent,
});

const onSubmit = async (e: FormEvent) => {
  e.preventDefault();
  if (!input.trim()) return;
  const text = input;
  setInput("");
  await sendMessage({ role: "user", parts: [{ type: "text", text }] });
};
```

## Workers AI Provider (Recommended)

Use `workers-ai-provider` with the AI SDK:

```typescript
import { createWorkersAI } from "workers-ai-provider";
import { streamText, convertToModelMessages } from "ai";

const workersai = createWorkersAI({ binding: this.env.AI });

const result = streamText({
  model: workersai("auto", {}),
  messages: await convertToModelMessages(this.messages),
  system: "You are a helpful AI assistant.",
});
```

Notes:
- `"auto"` is one of four model tiers (`cheap`, `fast`, `auto`, `smart`) — see [Model Tiers](#model-tiers) below.
- The platform resolves tier names to a concrete model based on the org's active AI provider (Anthropic / OpenAI / Bedrock / OpenRouter); orgs without a configured key use OpenRouter via camelAI-managed credits.
- Do not set `max_tokens` by default. Thinking/reasoning tokens consume that same budget and can truncate completions prematurely.
- If you must use `max_tokens`, leave substantial headroom for both thinking and final output.

## Tools and Agents

> **IMPORTANT: Always use codemode for tool-calling agents.** Codemode is the **strongly recommended default** for any agent that uses tools. It lets the LLM orchestrate multiple tools in a single turn by writing TypeScript code — calling tools sequentially, in parallel, with conditionals, loops, and error handling — all within one step. This is dramatically faster, more reliable, and more capable than plain tool calling, which forces one-tool-at-a-time round-trips with the model.
>
> **Only fall back to plain tool calling** if the agent has a single, trivially simple tool with no chaining needs. In every other case, **use codemode.**

### Why Codemode Over Plain Tool Calling

| | Codemode | Plain Tool Calling |
|---|---|---|
| **Tools per turn** | Unlimited — chain, branch, loop | One at a time, sequential round-trips |
| **Latency** | One LLM call orchestrates many tools | N tools = N+ LLM round-trips |
| **Logic** | Full TypeScript: conditionals, loops, try/catch | Model must "reason" across turns |
| **Reliability** | Code executes deterministically | Model may lose track of multi-step plans |
| **Type safety** | `outputSchema` → real TS types in generated code | Tool outputs are untyped `unknown` |

### Codemode Setup

The starter template has codemode pre-configured (commented out in `workers/chat.ts`). Uncomment and customize:

1. Define tools with `outputSchema` for typed code generation
2. Create executor: `new DynamicWorkerExecutor({ loader: this.env.LOADER })`
3. Create code tool: `createCodeTool({ tools: myTools, executor })`
4. Pass to streamText: `tools: { codemode: codeTool }`
5. Always set `stopWhen: stepCountIs(100)` for multi-step tool use

### Codemode Tool Design — Only Provide Data Tools

Do **NOT** create "pass-through" or "formatting" tools (e.g., a `createVisualization` tool that just echoes inputs into a chart config). Only wrap tools that **access data** or **perform side effects**. The LLM handles all data transformation and output shaping in its generated code.

### Codemode Return Type Convention

> This pattern serves double duty: it provides **structured output** (replacing `Output.object()` which doesn't work with the Workers AI provider + tools) AND tells the frontend how to render results. Define your return types here — not in `Output.object()`.

Define a discriminated return type convention in your `createCodeTool` description so the frontend knows how to render each result. Use a `type` field as the discriminator. Include the `{{types}}` placeholder so the LLM sees the tool type definitions, and provide concrete examples:

```typescript
const codemode = createCodeTool({
  tools: dataTools,
  executor,
  description: `Execute code to query and analyze data. You have access to these tools via the \`codemode\` object:

{{types}}

IMPORTANT: Your code MUST return a result object with a "type" field indicating what to render:

1. For CHARTS — return:
   { type: "chart", chartType: "bar"|"line"|"area"|"pie", title: string, data: Array<Record<string, any>>, xKey: string, yKeys: string[], xLabel?: string, yLabel?: string }

2. For TABLES — return:
   { type: "table", companies: Array<...>, total: number, showing: number }

3. For RAW STATS (no visual) — return:
   { type: "stats", stats: Array<{ label: string, value: number }>, groupBy: string, metric: string }

Examples:

// Bar chart
async () => {
  const result = await codemode.aggregateStats({ groupBy: "category", metric: "count", limit: 10 });
  return {
    type: "chart",
    chartType: "bar",
    title: "Top 10 Categories",
    data: result.stats.map(s => ({ label: s.label, value: s.value })),
    xKey: "label",
    yKeys: ["value"],
    xLabel: "Category",
    yLabel: "Count"
  };
}`,
});
```

### Codemode Frontend Rendering — AI SDK UIMessage Part Format

> **This is the #1 source of bugs when integrating codemode.** The AI SDK v5+ uses a different UIMessage part format than what older docs describe.

The starter template's `chat.tsx` has a working implementation. Key differences from older docs:

| Property | Old SDK (pre-v5) | Current SDK (v5+) |
|----------|-------------------|---------------------|
| Part type | `p.type === "tool-invocation"` | `p.type === "tool-{toolName}"` (e.g., `"tool-codemode"`) |
| Completion state | `p.state === "result"` | `p.state === "output-available"` |
| Result data | `p.result` | `p.output` |
| Tool name | `p.toolName` | `undefined` — extract from `p.type.replace("tool-", "")` |
| Error state | N/A | `p.state === "output-error"` |

**Codemode output shape** (what `p.output` contains):
```typescript
{ code: "async () => { ... }", result: { type: "chart", ... }, logs: [] }
```

The frontend reads `p.output.result.type` to decide what to render.

**Key gotchas:**
1. `p.type` is `"tool-codemode"`, NOT `"tool-invocation"` — always use `p.type.startsWith("tool-")`
2. The tool name comes from `p.type`, not `p.toolName` (which is `undefined` in the new SDK)
3. Codemode wraps the LLM's return value — the actual data is in `result.result`, not `result`
4. Use `p.output ?? p.result` to handle both old and new SDK versions
5. Check for `"output-available"` state, not just `"result"`
6. **Blank bubble gap** — The assistant message stream starts before any parts arrive. If your loading indicator only checks `lastMessage.role !== "assistant"`, it will hide too early. Use `hasVisibleContent()` (see `chat.tsx`) before hiding the loading state.

### Zod Parameter Defensive Defaults

Tool parameters validated with Zod may arrive as `undefined` at runtime (Zod v3/v4 compatibility gap with the `ai` package). Always add defensive defaults using `??` (not `||`, which replaces valid falsy values like `0`, `false`, `""`):

```typescript
execute: async (params) => {
  const metric = params.metric ?? "count";
  const groupBy = params.groupBy ?? "industry";
  return computeStats({ metric, groupBy });
},
```

## Structured Output from Agents

### `Output.object()` Limitation

> **`Output.object()` does not work with the Workers AI provider when tools are present.** The `workers-ai-provider` strips tools from the request when `response_format` is `json_schema`, making structured output and tool calling mutually exclusive. The AI Gateway also does not enforce `json_schema` on downstream models, so even structured-only requests (no tools) fail.
>
> **Use codemode return type conventions instead.** The LLM writes TypeScript that constructs and returns a typed object — this is more reliable than LLM JSON generation because the structure comes from executed code, not text completion.

### Backend Extraction Pattern

When using codemode for structured output in API routes (not chat streaming), extract the result from tool steps:

```typescript
import { generateText, stepCountIs } from "ai";

const result = await generateText({
  model: workersai("auto", {}),
  tools: { codemode: codeTool },
  stopWhen: stepCountIs(100),
  system: "Use codemode to query data and return structured results.",
  prompt,
});

// Extract structured result from codemode output
let structuredResult = null;
for (const step of result.steps) {
  if (step.toolResults?.length) {
    for (const tr of step.toolResults) {
      const output = (tr as any).output ?? (tr as any).result;
      if (output?.result) {
        structuredResult = output.result; // Your discriminated return value
      }
    }
  }
}
```

## Stateless Route Example

```typescript
import { data } from "react-router";
import { generateText } from "ai";
import { createWorkersAI } from "workers-ai-provider";

export async function action({ request, context }) {
  const { prompt } = await request.json();
  const workersai = createWorkersAI({ binding: context.cloudflare.env.AI });

  const { text } = await generateText({
    model: workersai("auto", {}),
    prompt,
  });

  return data({ response: text });
}
```

## Codemode Reference

`@cloudflare/codemode` is pre-configured in the starter template (`worker_loaders` + `SELF` bindings in `wrangler.jsonc`). See `workers/chat.ts` for setup.

### How It Works

1. You define tools with `outputSchema` so the generated code gets proper types
2. `createCodeTool` wraps your tools into a single "code" tool the LLM can call
3. `DynamicWorkerExecutor` runs the LLM-generated code in an ephemeral Worker isolate
4. The LLM writes code like `const weather = await getWeather({ city: "Paris" }); return await formatReport(weather);`

### Key Points

- **`outputSchema`** on tools produces real TypeScript types instead of `unknown` in generated code
- **`env.LOADER`** (`worker_loaders` binding) provides the isolate runtime
- The `__filename` define in `vite.config.ts` polyfills a Node.js global needed by the TypeScript compiler

## Model Tiers

Pass one of four tier names to `workersai(tier, {})`. The platform resolves the tier to a concrete model based on the org's active AI provider:

| Tier | Purpose | When to Use |
|------|---------|-------------|
| `cheap` | Cheapest small model | High-volume, low-stakes work — classification, simple extraction, short replies |
| `fast` | Same low-latency model as `cheap` | When latency matters more than reasoning depth |
| `auto` | Balanced default | General-purpose AI features — the right pick when you're unsure |
| `smart` | Strongest reasoning model | Complex reasoning, long-context analysis, agentic tool use |

**Always default to `auto`** unless the use case clearly needs a smaller/cheaper tier or the strongest reasoning. Tier resolution is per-provider, so the same `auto` call lands on Sonnet for an Anthropic org, GPT mini for an OpenAI org, Claude on Bedrock for a Bedrock org, and Kimi K2.6 (via OpenRouter) for hosted-credit orgs.

### Specific OpenRouter Models (Only When User Explicitly Requests)

Any model available on [OpenRouter](https://openrouter.ai/models) can be used by passing the full model identifier (e.g., `anthropic/claude-sonnet-4.6`, `openai/gpt-5.5`, `google/gemini-3.5-flash`). The platform appends `:nitro` automatically so requests route through OpenRouter's fastest provider. **Only use a specific model when the user explicitly asks for it.** Never proactively choose a specific model — tier routing is always the better default.

```typescript
const result = await streamText({
  model: workersai("anthropic/claude-sonnet-4.6", {}),
  messages: await convertToModelMessages(this.messages),
});
```

Model selection is supported in deployed workers via `env.AI`.

#### Discovering Available Models

To look up available models, pricing, or capabilities, fetch the OpenRouter models API (no auth required):

```bash
curl -s https://openrouter.ai/api/v1/models
```

Returns a `data[]` array where each entry has:

| Field | Description |
|-------|-------------|
| `id` | Model identifier to pass as the model param (e.g., `anthropic/claude-sonnet-4.6`) |
| `name` | Human-readable name (e.g., `Anthropic: Claude Sonnet 4.6`) |
| `context_length` | Max context window in tokens |
| `pricing.prompt` | Cost per input token (USD) |
| `pricing.completion` | Cost per output token (USD) |
| `architecture.modality` | Input/output modalities (e.g., `text+image->text`) |
| `supported_parameters` | Supported API params (e.g., `tools`, `response_format`) |

Use this when a user asks what models are available, wants to compare pricing, or needs a model with specific capabilities (e.g., tool calling, large context, vision).

### Image Generation Example

> Image generation is not a tier on the `AI` binding — call `env.CAMELAI.generateImage(...)` instead. The `workers-ai-provider` strips the `images` array from chat completions, so `generateText()` would only return the text portion.

```typescript
const { text, imageDataUrl, images } = await context.cloudflare.env.CAMELAI.generateImage(
  "Generate a watercolor mountain landscape",
);
// imageDataUrl is "data:image/png;base64,..." when the model returns an image
```

Optional style reference:

```typescript
const result = await context.cloudflare.env.CAMELAI.generateImage({
  prompt: "Generate a new image in the same visual style, different subject",
  referenceImageUrl: existingDataUrl,
});
```

## Best Practices

1. **Always pass a unique `name` to `useAgent`** — without it, all users share one DO instance. Generate session IDs server-side in loaders.
2. **Manage your own input state** — `useAgentChat` does not return `input`/`setInput`/`handleSubmit` in AI SDK v3. Use `useState` + `sendMessage`.
3. **Always use codemode for tool-calling agents** — lets the LLM chain, branch, and parallelize tool calls in one turn. Only skip for a single trivially simple tool.
4. **Add `outputSchema` to every tool** — generates real TypeScript types in codemode.
5. **Only wrap data/side-effect tools in codemode** — the LLM constructs output shapes directly in code.
6. **Use a `type` discriminator in codemode return values** — define the convention in your `createCodeTool` description.
7. **Handle the current AI SDK part format** — use `p.type.startsWith("tool-")`, `p.output ?? p.result`, and `state === "output-available"`.
8. **Use `??` for defensive defaults** — Zod params may be `undefined` at runtime. `||` silently replaces valid `0`/`false`/`""`.
9. Use `workersai("auto", {})` as the default model. Pick `cheap`/`fast` for high-volume low-stakes work, `smart` for hard reasoning. Only use a specific OpenRouter model (e.g., `"anthropic/claude-sonnet-4.6"`) when the user explicitly requests it.
10. Avoid `max_tokens` unless a hard cap is required; reasoning tokens count toward it.
11. Stream responses for chat UX.
12. Use `MarkdownRenderer` for assistant output.
