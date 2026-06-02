import { createRequestHandler } from "react-router";
// import { routeAgentRequest } from "agents";

// Export Durable Objects so Cloudflare can instantiate them
// Add new DOs here after creating them in workers/
export { ExampleDO } from "./example-do";
export { LocalDataProxyService } from "./data-proxy";
export { LocalConnectionsService } from "./connections";
export { LocalCamelAiService } from "./camelai-service";
// export { ChatSessionsDO } from "./chat-sessions";
// export { Chat } from "./chat";

/**
 * Augment AppLoadContext to include Cloudflare bindings.
 * Access in loaders/actions via: context.cloudflare.env.BINDING_NAME
 */
declare module "react-router" {
  export interface AppLoadContext {
    cloudflare: {
      env: Env;
      ctx: ExecutionContext;
      // Uncomment when enabling AI chat (see CLAUDE.md):
      // ownerId: string;
    };
  }
}

const requestHandler = createRequestHandler(
  () => import("virtual:react-router/server-build"),
  import.meta.env.MODE
);

/**
 * Parse a named cookie from a Cookie header string.
 */
function getCookie(request: Request, name: string): string | undefined {
  const header = request.headers.get("Cookie") ?? "";
  const match = header.match(new RegExp(`(?:^|;\\s*)${name}=([^;]*)`));
  return match?.[1];
}

export default {
  async fetch(request, env, ctx) {
    /* Uncomment to enable Agents SDK routing (WebSocket for Chat DO)
    const agentResponse = await routeAgentRequest(request, env);
    if (agentResponse) {
      return agentResponse;
    }
    */

    /* Uncomment to enable anonymous chat sessions (chat-owner cookie):
    let ownerId = getCookie(request, "chat-owner");
    const needsCookie = !ownerId;
    if (!ownerId) {
      ownerId = crypto.randomUUID();
    }
    */

    // Handle all requests with React Router SSR
    return requestHandler(request, {
      cloudflare: { env, ctx },
    });

    /* Uncomment (and remove the plain return above) to set the chat-owner
       cookie on new visitors:
    const response = await requestHandler(request, {
      cloudflare: { env, ctx, ownerId },
    });
    if (needsCookie) {
      const newResponse = new Response(response.body, response);
      newResponse.headers.append(
        "Set-Cookie",
        `chat-owner=${ownerId}; Path=/; HttpOnly; SameSite=Lax; Max-Age=31536000`
      );
      return newResponse;
    }
    return response;
    */
  },
} satisfies ExportedHandler<Env>;
