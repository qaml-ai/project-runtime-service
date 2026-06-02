---
name: deterministic-automations
description: Create and maintain durable deterministic automations (known as workflows) for the current workspace using Cloudflare Dynamic Workflows. Use when the user asks for scheduled code that should run without another model turn, durable sleeps/retries, workflow steps, or automation scripts.
license: Complete terms in LICENSE.txt
---

# Workflows

Workflows are workspace-scoped Cloudflare Dynamic Workflow scripts. They run deterministic JavaScript code on a UTC cron schedule without sending a prompt to a chat thread.

Use agent tasks when the user wants the agent to think or write a reply later. Use workflows when the user wants predictable code execution, durable workflow steps, retries, sleeps, or event waits.

## Tools

- `list_workflows` - list workspace workflows and their virtual source paths.
- `validate_workflow` - check source syntax and required exports before saving.
- `create_workflow` - create a workflow with `{ name, source, cron_expression, description, enabled? }`. Description is required and should summarize what the workflow does.
- `update_workflow` - update metadata, schedule, enabled state, or source with `{ workflow_id, ... }`.
- `delete_workflow` - delete the schedule. Already-started workflow instances may still need their versioned source.
- `run_workflow_now` - start the workflow immediately.

Workflow scripts are also exposed as virtual files:

```text
/home/claude/.camelai/automations/<workflow_id>.js
```

After creating a workflow, use `read`, `edit`, or `write` on that path to inspect or update the script like a normal file. New workflows must be created with `create_workflow` because the schedule and display name are metadata, not file contents.

## Script Shape

Every script must export `AutomationWorkflow`:

```js
import { WorkflowEntrypoint } from "cloudflare:workers";

export class AutomationWorkflow extends WorkflowEntrypoint {
  async run(event, step) {
    const payload = event.payload;

    await step.do("record run", async () => {
      console.log("Workflow fired", payload);
    });

    return { ok: true, firedAt: payload.triggeredAt };
  }
}
```

The workflow receives `event.payload` with:

```js
{
  workspaceId,
  workflowId,
  workflowName,
  scheduledFor,   // ISO timestamp for the scheduled slot
  triggeredAt,    // ISO timestamp when the workflow instance was created
  trigger         // "schedule" or "manual"
}
```

Legacy aliases `automationId` and `automationName` are also present in the payload.

## Available Bindings

Inside `AutomationWorkflow`, use `this.env`:

- `this.env.CONNECTIONS` - workspace connection binding.
- `this.env.AI` - virtual AI binding when deterministic code needs a model call.
- `this.env.TOOLS` - harness tool binding for non-interactive platform tools.

Prefer deterministic connection calls over agent-like orchestration:

```js
import { WorkflowEntrypoint } from "cloudflare:workers";

export class AutomationWorkflow extends WorkflowEntrypoint {
  async run(event, step) {
    const result = await step.do("query database", async () => {
      const db = await this.env.CONNECTIONS.find("postgres");
      const connections = this.env.CONNECTIONS;
      return await connections[db.alias].query({
        query: "SELECT count(*) AS total FROM users",
      });
    });

    return { users: result };
  }
}
```

## Durable Workflow Patterns

Use `step.do` for side effects and retryable units:

```js
await step.do("sync external API", { retries: { limit: 3, delay: "30 seconds" } }, async () => {
  const response = await fetch("https://api.example.com/sync", { method: "POST" });
  if (!response.ok) throw new Error(`Sync failed: ${response.status}`);
  return await response.json();
});
```

Use `step.sleep` for long waits that should survive Worker restarts:

```js
await step.sleep("wait for settlement", "2 hours");
await step.do("check status", async () => {
  // Follow-up work here.
});
```

Use `step.waitForEvent` only when the app has a clear way to send the event to the workflow instance:

```js
const approval = await step.waitForEvent("wait for approval", {
  type: "approval.received",
  timeout: "7 days",
});
```

## Create Workflow

1. Write the source.
2. Call `validate_workflow` with the source.
3. Call `create_workflow`.
4. Call `run_workflow_now` for a smoke test when the action is safe.

Example:

```js
const source = `import { WorkflowEntrypoint } from "cloudflare:workers";

export class AutomationWorkflow extends WorkflowEntrypoint {
  async run(event, step) {
    return await step.do("daily health check", async () => {
      const response = await fetch("https://example.com/health");
      return { ok: response.ok, status: response.status, scheduledFor: event.payload.scheduledFor };
    });
  }
}
`;

await tools.validate_workflow({ source });
await tools.create_workflow({
  name: "Daily health check",
  description: "Fetches the public health endpoint every morning.",
  source,
  cron_expression: "0 14 * * *",
});
```

Cron expressions are 5 fields in UTC: `minute hour day-of-month month day-of-week`.

## Editing

```js
await tools.list_workflows({});
await tools.read({ path: "/home/claude/.camelai/automations/<workflow_id>.js" });
await tools.edit({
  path: "/home/claude/.camelai/automations/<workflow_id>.js",
  edits: [{ oldText: "old code", newText: "new code" }],
});
```

Each source edit creates a new source version. Started workflow instances run against the version they were created with.
