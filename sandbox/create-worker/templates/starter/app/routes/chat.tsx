import { useState, useRef, type FormEvent, useEffect, useCallback } from "react";
import {
  useLoaderData,
  redirect,
  useNavigate,
  useFetcher,
} from "react-router";
import { useAgent } from "agents/react";
import { useAgentChat } from "@cloudflare/ai-chat/react";
import type { UIMessage } from "@ai-sdk/react";
import { MarkdownRenderer } from "~/components/markdown-renderer";
import type { Route } from "./+types/chat";
import type { ChatSessionRow } from "../../workers/chat-sessions";

/**
 * Chat route using Cloudflare Agents SDK.
 *
 * IMPORTANT: Always use MarkdownRenderer for AI responses!
 * AI models return markdown (code blocks, lists, tables) that must be
 * rendered properly. Never use plain <p> tags for assistant messages.
 *
 * Key features:
 * - Real-time streaming responses via WebSocket
 * - Automatic conversation history persistence (SQLite in Durable Object)
 * - Resumable streaming (reconnects continue where they left off)
 * - Full conversation context passed to AI on every message
 * - Chat history sidebar backed by a ChatSessionsDO (per anonymous user)
 *
 * How conversation continuity works:
 * - The Chat Durable Object stores all messages in SQLite via `this.messages`
 * - On each new message, ALL previous messages are passed to the AI model
 * - This happens automatically via `convertToModelMessages(this.messages)` in chat.ts
 * - The SDK handles persistence, so conversations survive page refreshes
 *
 * Session isolation:
 * - Each unique `name` in useAgent creates a separate Durable Object instance
 * - WITHOUT a unique name, ALL users share the same DO ("default") and see
 *   each other's conversations — this is the #1 deployment bug
 * - The session ID lives in the URL (/chat?session=<id>) so users can have
 *   multiple conversations and share/bookmark them
 * - Visiting /chat with no ?session redirects to a fresh session automatically
 *
 * Anonymous ownership:
 * - A `chat-owner` cookie (set in workers/app.ts) identifies the browser
 * - A ChatSessionsDO keyed by that cookie stores the session index
 * - No auth required — swap cookie for a real user ID when you add login
 *
 * API notes (AI SDK v3):
 * - useAgentChat does NOT return input/setInput/handleSubmit — manage your
 *   own input state with useState and use sendMessage() to send
 * - sendMessage accepts { text } shorthand or { role, parts } for rich content
 */

// --- Helpers ---

function getSessionsStub(context: Route.LoaderArgs["context"]) {
  const { env, ownerId } = context.cloudflare;
  // Cast: CHAT_SESSIONS binding is commented out by default in wrangler.jsonc;
  // it's only present when the AI chat feature is enabled (see CLAUDE.md).
  const binding = (env as any).CHAT_SESSIONS as DurableObjectNamespace;
  const doId = binding.idFromName(ownerId);
  return binding.get(doId);
}

// --- Loader ---

/**
 * Creates a new session and redirects when no ?session= is present.
 * When a session ID is in the URL, verifies it belongs to the current
 * owner — unknown IDs redirect to a new chat instead of auto-claiming.
 * This prevents an attacker from "adopting" another owner's session ID
 * into their index and then deleting it to wipe the Chat DO.
 */
export async function loader({ request, context }: Route.LoaderArgs) {
  const url = new URL(request.url);
  const sessionId = url.searchParams.get("session");
  const stub = getSessionsStub(context);

  // No session in URL → create one in the index and redirect
  if (!sessionId) {
    const newId = crypto.randomUUID();
    await stub.ensureSession(newId);
    url.searchParams.set("session", newId);
    throw redirect(url.pathname + url.search);
  }

  const sessions = await stub.listSessions();
  const isOwned = sessions.some((s) => s.id === sessionId);

  if (!isOwned) {
    // Unknown session ID — don't auto-register it; redirect to a new chat
    url.searchParams.delete("session");
    throw redirect(url.pathname);
  }

  return { sessionId, sessions };
}

// --- Action ---

/**
 * Handles session mutations: delete and update-title.
 * Uses React Router `<Form>` / `useFetcher` for server-driven mutations.
 */
export async function action({ request, context }: Route.ActionArgs) {
  const formData = await request.formData();
  const intent = formData.get("intent");
  const stub = getSessionsStub(context);

  if (intent === "delete") {
    const id = formData.get("id") as string;
    if (id) {
      // deleteSession returns true only if the session belonged to this owner.
      // This prevents a crafted request from wiping another owner's Chat DO.
      const deleted = await stub.deleteSession(id);
      if (deleted) {
        // Also clear the Chat DO's messages so the conversation is fully removed.
        // The Chat binding exists only when the AI chat feature is enabled.
        const chatBinding = (context.cloudflare.env as any).Chat as
          | DurableObjectNamespace
          | undefined;
        if (chatBinding) {
          try {
            const chatStub = chatBinding.get(chatBinding.idFromName(id));
            await (chatStub as any).deleteAllMessages();
          } catch {
            // Chat DO not available — index entry is still removed
          }
        }
      }
    }
    return { ok: true };
  }

  if (intent === "update-title") {
    const id = formData.get("id") as string;
    const title = formData.get("title") as string;
    if (id && title) await stub.updateTitle(id, title);
    return { ok: true };
  }

  return { ok: false };
}

// --- Components ---

export default function ChatPage() {
  const { sessionId, sessions } = useLoaderData<typeof loader>();
  const [mounted, setMounted] = useState(false);

  useEffect(() => {
    setMounted(true);
  }, []);

  if (!mounted) {
    return (
      <div className="flex h-screen bg-gray-50">
        <div className="w-64 bg-gray-900" />
        <div className="flex-1 flex items-center justify-center">
          <p className="text-gray-500">Loading chat...</p>
        </div>
      </div>
    );
  }

  return (
    <ChatWithSidebar sessionId={sessionId} initialSessions={sessions} />
  );
}

function ChatWithSidebar({
  sessionId,
  initialSessions,
}: {
  sessionId: string;
  initialSessions: ChatSessionRow[];
}) {
  const navigate = useNavigate();
  const fetcher = useFetcher();
  const [sidebarOpen, setSidebarOpen] = useState(true);

  // Use fetcher data to get updated sessions after mutations,
  // falling back to loader data
  const loaderData = useLoaderData<typeof loader>();
  const sessions = loaderData.sessions ?? initialSessions;

  const handleNewChat = () => {
    // Navigate without a session param — the loader creates the session
    // in the owner's index and redirects to /chat?session=<newId>
    navigate(`/chat`);
  };

  const handleSelectSession = (id: string) => {
    navigate(`/chat?session=${id}`);
  };

  const handleDeleteSession = (id: string) => {
    fetcher.submit({ intent: "delete", id }, { method: "post" });
    // If deleting the active session, navigate to a new chat
    if (id === sessionId) {
      handleNewChat();
    }
  };

  const handleTitleUpdate = useCallback(
    (title: string) => {
      fetcher.submit(
        { intent: "update-title", id: sessionId, title },
        { method: "post" }
      );
    },
    [sessionId, fetcher]
  );

  // Sort sessions by most recently updated
  const sortedSessions = [...sessions].sort(
    (a, b) =>
      new Date(b.updated_at).getTime() - new Date(a.updated_at).getTime()
  );

  return (
    <div className="flex h-screen bg-gray-50">
      {/* Sidebar */}
      <div
        className={`${
          sidebarOpen ? "w-64" : "w-0"
        } bg-gray-900 text-white flex flex-col transition-all duration-200 overflow-hidden`}
      >
        {/* New Chat button */}
        <div className="p-3">
          <button
            onClick={handleNewChat}
            className="w-full flex items-center gap-2 px-3 py-2 rounded-lg border border-gray-700 hover:bg-gray-800 transition-colors text-sm"
          >
            <svg
              width="16"
              height="16"
              viewBox="0 0 16 16"
              fill="none"
              stroke="currentColor"
              strokeWidth="2"
            >
              <line x1="8" y1="3" x2="8" y2="13" />
              <line x1="3" y1="8" x2="13" y2="8" />
            </svg>
            New Chat
          </button>
        </div>

        {/* Session list */}
        <nav className="flex-1 overflow-y-auto px-2 pb-3 space-y-0.5">
          {sortedSessions.map((session) => (
            <div
              key={session.id}
              className={`group flex items-center rounded-lg px-3 py-2 text-sm cursor-pointer ${
                session.id === sessionId
                  ? "bg-gray-700 text-white"
                  : "text-gray-300 hover:bg-gray-800 hover:text-white"
              }`}
              onClick={() => handleSelectSession(session.id)}
            >
              <span className="flex-1 truncate">{session.title}</span>
              <button
                onClick={(e) => {
                  e.stopPropagation();
                  handleDeleteSession(session.id);
                }}
                className="opacity-0 group-hover:opacity-100 ml-2 p-0.5 hover:text-red-400 transition-opacity"
                title="Delete chat"
              >
                <svg
                  width="14"
                  height="14"
                  viewBox="0 0 14 14"
                  fill="none"
                  stroke="currentColor"
                  strokeWidth="1.5"
                >
                  <line x1="3" y1="3" x2="11" y2="11" />
                  <line x1="11" y1="3" x2="3" y2="11" />
                </svg>
              </button>
            </div>
          ))}
        </nav>
      </div>

      {/* Main chat area */}
      <div className="flex-1 flex flex-col min-w-0">
        <ChatClient
          sessionId={sessionId}
          sidebarOpen={sidebarOpen}
          onToggleSidebar={() => setSidebarOpen(!sidebarOpen)}
          onTitleUpdate={handleTitleUpdate}
        />
      </div>
    </div>
  );
}

function ChatClient({
  sessionId,
  sidebarOpen,
  onToggleSidebar,
  onTitleUpdate,
}: {
  sessionId: string;
  sidebarOpen: boolean;
  onToggleSidebar: () => void;
  onTitleUpdate: (title: string) => void;
}) {
  const [input, setInput] = useState("");
  const [titleSet, setTitleSet] = useState(false);
  const initialMessageCountRef = useRef<number | null>(null);

  // Reset title tracking when session changes
  useEffect(() => {
    setTitleSet(false);
    initialMessageCountRef.current = null;
  }, [sessionId]);

  // IMPORTANT: Always pass a unique `name` to useAgent.
  // Without it, every user shares the same Durable Object instance ("default")
  // and sees each other's conversations.
  const agent = useAgent({
    agent: "Chat",
    name: sessionId,
  });

  // Note: useAgentChat does NOT return input/setInput/handleSubmit (removed
  // in AI SDK v3). Manage your own input state with useState.
  const { messages, sendMessage, status, error, clearHistory } = useAgentChat({
    agent,
  });

  const isLoading = status === "streaming" || status === "submitted";
  const isStreaming = status === "streaming";

  // Update sidebar title from the first user message, but only when a new
  // message is actually sent — not when reopening an existing session whose
  // messages are replayed from the DO. This prevents unnecessary title-update
  // POSTs (which bump updated_at) on every session switch.
  useEffect(() => {
    if (titleSet) return;
    if (initialMessageCountRef.current === null) {
      // Capture the initial message count on first render / session switch
      initialMessageCountRef.current = messages.length;
      return;
    }
    // Only update title when new messages arrive beyond what was initially loaded
    if (messages.length <= initialMessageCountRef.current) return;
    const firstUserMsg = messages.find((m) => m.role === "user");
    if (firstUserMsg) {
      const text = firstUserMsg.parts
        ?.filter(
          (p: any): p is { type: "text"; text: string } => p.type === "text"
        )
        .map((p: any) => p.text)
        .join("");
      if (text) {
        onTitleUpdate(text.length > 40 ? text.slice(0, 40) + "..." : text);
        setTitleSet(true);
      }
    }
  }, [messages, titleSet, onTitleUpdate]);

  const onSubmit = async (e: FormEvent) => {
    e.preventDefault();
    if (!input.trim() || isLoading) return;

    const text = input;
    setInput("");

    // Using the parts format makes it easy to extend with images, files, etc.
    // For text-only, you can also use the shorthand: sendMessage({ text })
    await sendMessage({
      role: "user",
      parts: [{ type: "text", text }],
    });
  };

  return (
    <>
      {/* Header */}
      <header className="bg-white border-b border-gray-200 px-4 py-3 flex items-center gap-3">
        <button
          onClick={onToggleSidebar}
          className="p-1 rounded hover:bg-gray-100 text-gray-500 hover:text-gray-700 transition-colors"
          title={sidebarOpen ? "Close sidebar" : "Open sidebar"}
        >
          <svg
            width="20"
            height="20"
            viewBox="0 0 20 20"
            fill="none"
            stroke="currentColor"
            strokeWidth="1.5"
          >
            <line x1="3" y1="5" x2="17" y2="5" />
            <line x1="3" y1="10" x2="17" y2="10" />
            <line x1="3" y1="15" x2="17" y2="15" />
          </svg>
        </button>
        <h1 className="text-xl font-semibold text-gray-900">AI Chat</h1>
        <div className="flex-1" />
        <button
          onClick={clearHistory}
          className="text-sm text-gray-500 hover:text-gray-700"
        >
          Clear
        </button>
      </header>

      {/* Messages */}
      <main className="flex-1 overflow-y-auto p-4 space-y-4">
        {messages.length === 0 && (
          <div className="text-center text-gray-500 mt-8">
            <p className="text-lg">Start a conversation</p>
            <p className="text-sm mt-2">
              Type a message below to chat with the AI
            </p>
          </div>
        )}

        {messages.map((message: UIMessage, index: number) => {
          const isLastMessage = index === messages.length - 1;
          const isAssistant = message.role === "assistant";

          return (
            <div
              key={message.id}
              className={`flex ${isAssistant ? "justify-start" : "justify-end"}`}
            >
              <div
                className={`max-w-[80%] rounded-lg px-4 py-2 ${
                  isAssistant
                    ? "bg-white border border-gray-200 text-gray-900"
                    : "bg-blue-600 text-white"
                }`}
              >
                {isAssistant ? (
                  <>
                    {/* Render all message parts — text and tool results */}
                    {message.parts?.map((part: any, i: number) => {
                      // Text parts — always use MarkdownRenderer for AI output
                      if (part.type === "text" && part.text) {
                        return (
                          <MarkdownRenderer
                            key={i}
                            content={part.text}
                            isStreaming={isStreaming && isLastMessage}
                          />
                        );
                      }
                      return null;
                    })}
                    {/* Show loading dots on the last assistant message while streaming */}
                    {isLoading && isLastMessage && (
                      <div className="flex space-x-1 py-1">
                        <div className="w-2 h-2 bg-gray-400 rounded-full animate-bounce" />
                        <div className="w-2 h-2 bg-gray-400 rounded-full animate-bounce [animation-delay:0.1s]" />
                        <div className="w-2 h-2 bg-gray-400 rounded-full animate-bounce [animation-delay:0.2s]" />
                      </div>
                    )}
                  </>
                ) : (
                  <p className="whitespace-pre-wrap">
                    {message.parts
                      ?.filter(
                        (p: any): p is { type: "text"; text: string } =>
                          p.type === "text"
                      )
                      .map((p: any) => p.text)
                      .join("")}
                  </p>
                )}
              </div>
            </div>
          );
        })}

        {/* Show loading bubble when waiting and no assistant message exists yet */}
        {isLoading &&
          messages[messages.length - 1]?.role !== "assistant" && (
            <div className="flex justify-start">
              <div className="bg-white border border-gray-200 rounded-lg px-4 py-2">
                <div className="flex space-x-1">
                  <div className="w-2 h-2 bg-gray-400 rounded-full animate-bounce" />
                  <div className="w-2 h-2 bg-gray-400 rounded-full animate-bounce [animation-delay:0.1s]" />
                  <div className="w-2 h-2 bg-gray-400 rounded-full animate-bounce [animation-delay:0.2s]" />
                </div>
              </div>
            </div>
          )}

        {error && (
          <div className="bg-red-50 border border-red-200 rounded-lg px-4 py-2 text-red-700">
            Error: {error.message}
          </div>
        )}
      </main>

      {/* Input */}
      <footer className="bg-white border-t border-gray-200 p-4">
        <form onSubmit={onSubmit} className="flex gap-2">
          <input
            type="text"
            value={input}
            onChange={(e) => setInput(e.target.value)}
            placeholder="Type a message..."
            className="flex-1 rounded-lg border border-gray-300 px-4 py-2 focus:outline-none focus:ring-2 focus:ring-blue-500 focus:border-transparent"
            disabled={isLoading}
          />
          <button
            type="submit"
            disabled={isLoading || !input.trim()}
            className="bg-blue-600 text-white px-4 py-2 rounded-lg hover:bg-blue-700 disabled:opacity-50 disabled:cursor-not-allowed transition-colors"
          >
            Send
          </button>
        </form>
      </footer>
    </>
  );
}
