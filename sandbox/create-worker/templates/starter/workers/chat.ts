import { AIChatAgent } from "@cloudflare/ai-chat";
import {
  streamText,
  createUIMessageStream,
  createUIMessageStreamResponse,
  convertToModelMessages,
  stepCountIs,
  type StreamTextOnFinishCallback,
  type ToolSet,
  // tool,
} from "ai";
import { createWorkersAI } from "workers-ai-provider";
// import { z } from "zod";
// import { DynamicWorkerExecutor } from "@cloudflare/codemode";
// import { createCodeTool } from "@cloudflare/codemode/ai";

/**
 * Chat Agent Durable Object using Cloudflare Agents SDK.
 *
 * ## Features
 * - **Automatic conversation persistence**: Messages stored in SQLite via `this.messages`
 * - **Resumable streaming**: Reconnections continue from where they left off
 * - **WebSocket real-time communication**: Low-latency bidirectional messaging
 *
 * ## Conversation Continuity
 * The SDK automatically handles conversation history:
 *
 * 1. When a user sends a message, it's added to `this.messages`
 * 2. `convertToModelMessages(this.messages)` converts ALL messages to model format
 * 3. The full conversation history is passed to the AI on every request
 * 4. This allows the AI to maintain context across the entire conversation
 *
 * The messages array includes both user and assistant messages, so the AI
 * sees the complete back-and-forth of the conversation.
 *
 * ## Usage from React
 * ```tsx
 * // IMPORTANT: Always pass a unique `name` — without it, all users share
 * // one DO instance and see each other's conversations.
 * const agent = useAgent({ agent: "Chat", name: sessionId });
 *
 * // useAgentChat does NOT return input/setInput/handleSubmit (AI SDK v3).
 * // Manage your own input state with useState.
 * const { messages, sendMessage, status } = useAgentChat({ agent });
 * await sendMessage({ text: "Hello" });
 * ```
 */
export class Chat extends AIChatAgent<Env> {
  /** Clear all messages — called by the delete-session action. */
  async deleteAllMessages(): Promise<void> {
    this.messages = [];
  }

  /**
   * Called when a new message is received from the user.
   *
   * At this point, `this.messages` contains the FULL conversation history
   * including all previous user and assistant messages, plus the new user message.
   */
  async onChatMessage(
    onFinish: StreamTextOnFinishCallback<ToolSet>,
    options?: { abortSignal?: AbortSignal }
  ): Promise<Response> {
    const workersai = createWorkersAI({ binding: this.env.AI });

    /* --- Codemode (always use for agents with tools) ---
    // Codemode lets the LLM chain, branch, and parallelize tool calls in a
    // single turn by writing TypeScript code. Only skip codemode for agents
    // with a single trivially simple tool.
    //
    // For STRUCTURED OUTPUT: Use codemode return type conventions instead of
    // Output.object(), which doesn't work with the Workers AI provider + tools.
    const myTools = {
      getWeather: tool({
        description: "Get weather for a city",
        parameters: z.object({ city: z.string() }),
        // Always add outputSchema — generates real TS types in codemode
        outputSchema: z.object({ city: z.string(), temperature: z.number() }),
        execute: async ({ city }) => {
          // Use ?? (not ||) for defaults to preserve valid falsy values (0, false, "")
          const c = city ?? "Unknown";
          return { city: c, temperature: 72 };
        },
      }),
    };
    const executor = new DynamicWorkerExecutor({ loader: this.env.LOADER });
    const codeTool = createCodeTool({
      tools: myTools,
      executor,
      // Define return type conventions for structured output.
      // The LLM's generated code constructs and returns typed objects directly.
      description: `Execute code to query data. Tools available via \`codemode\`:

{{types}}

Your code MUST return a result object with a "type" field:

1. { type: "weather", city: string, temperature: number, summary: string }
2. { type: "comparison", cities: Array<{ city: string, temperature: number }>, warmest: string }`,
    });
    */

    const stream = createUIMessageStream({
      execute: async ({ writer }) => {
        const result = streamText({
          model: workersai("auto", {}),
          // Pass the FULL conversation history to maintain context
          // This includes all user messages and all previous AI responses
          messages: await convertToModelMessages(this.messages),
          system: "You are a helpful AI assistant.",
          // tools: { codemode: codeTool },
          // Enable multi-step tool use: without this, AI calls a tool but never
          // gets a chance to respond with text using the tool results (default is 1 step).
          stopWhen: stepCountIs(100),
          onFinish,
          abortSignal: options?.abortSignal,
        });
        writer.merge(result.toUIMessageStream());
      },
    });

    return createUIMessageStreamResponse({ stream });
  }
}
