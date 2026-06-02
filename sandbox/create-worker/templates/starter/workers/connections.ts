import { WorkerEntrypoint } from "cloudflare:workers";

interface LocalConnectionsEnv {
	CAMELAI_CONNECTIONS_URL?: string;
	MCP_SERVER_URL?: string;
}

export interface ConnectionSummary {
	id: string;
	type: string;
	name: string;
	displayName: string;
	category: string;
	authMethod: string;
	hasCredentials: boolean;
	capabilities: string[];
	nativeMcp: {
		serverName: string;
		transport: "streamable_http" | "sse";
		directConnect: boolean;
		brokered: boolean;
		authStrategy: string;
		preferredMode?: "direct" | "brokered";
		direct?: {
			serverName: string;
			url: string;
			transport: "streamable_http" | "sse";
			authStrategy: string;
			docsUrl?: string;
			notes?: string;
		};
		broker?: {
			serverName: string;
			url: string;
			transport: "streamable_http" | "sse";
			authStrategy: string;
			brokerPath: string;
			docsUrl?: string;
			notes?: string;
		};
	} | null;
}

export interface McpToolSummary {
	name: string;
	description?: string;
	inputSchema?: unknown;
	[key: string]: unknown;
}

export interface ConnectionMethodSummary {
	name: string;
	tool: string;
	description?: string;
	example?: string;
	inputSchema?: unknown;
	outputSchema?: unknown;
}

export interface ConnectionMethodCatalogEntry {
	alias: string;
	connection: ConnectionSummary;
	methods: ConnectionMethodSummary[];
	error?: {
		message: string;
		code?: unknown;
		data?: unknown;
	};
}

export interface ConnectionInvokeRequest {
	connection: string;
	method?: string;
	input?: unknown;
}

export type ConnectionFindQuery =
	| string
	| {
			id?: string;
			alias?: string;
			type?: string;
			name?: string;
	  };

const LEGACY_CONNECTION_INVOKE_METHOD = ["_", "_", "invoke"].join("");

function fallbackConnectionsUrl(env: LocalConnectionsEnv): string {
	const explicit = (env.CAMELAI_CONNECTIONS_URL ?? "").trim();
	if (explicit) return explicit.replace(/\/+$/, "");

	const mcpBase = (env.MCP_SERVER_URL ?? "").trim().replace(/\/+$/, "");
	if (mcpBase) return `${mcpBase.replace(/\/mcp$/, "")}/api/connections`;

	throw new Error("CAMELAI_CONNECTIONS_URL is not configured for local CONNECTIONS service");
}

async function request<T>(
	env: LocalConnectionsEnv,
	action: string,
	payload: Record<string, unknown> = {}
): Promise<T> {
	const response = await fetch(fallbackConnectionsUrl(env), {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify({ action, ...payload }),
	});

	const body = await response.json().catch(() => null) as { error?: unknown } | T | null;
	if (!response.ok) {
		throw new Error(
			typeof body === "object" && body && typeof body.error === "string"
				? body.error
				: `Local connections request failed (${response.status})`
		);
	}
	return body as T;
}

/**
 * Local CONNECTIONS shim used by the starter template.
 * Deploy pipeline rewrites this binding to the platform's internal ConnectionsService.
 */
export class LocalConnectionsService extends WorkerEntrypoint<LocalConnectionsEnv> {
	async list(): Promise<ConnectionSummary[]> {
		return request<ConnectionSummary[]>(this.env, "list");
	}

	async get(connection: string): Promise<ConnectionSummary> {
		return request<ConnectionSummary>(this.env, "get", { connection });
	}

	async tools(connection: string): Promise<McpToolSummary[]> {
		return request<McpToolSummary[]>(this.env, "tools", { connection });
	}

	/**
	 * Lists every workspace connection plus the method names and JSON schemas
	 * exposed on the method facade, e.g. `connections.stripeProd.listCustomers`.
	 */
	async methods(): Promise<ConnectionMethodCatalogEntry[]> {
		return request<ConnectionMethodCatalogEntry[]>(this.env, "methods");
	}

	async find(query: ConnectionFindQuery): Promise<ConnectionMethodCatalogEntry> {
		return request<ConnectionMethodCatalogEntry>(this.env, "find", { query });
	}

	async test(query: ConnectionFindQuery): Promise<unknown> {
		return request<unknown>(this.env, "test", { query });
	}

	async invoke<T = unknown>(invoke: ConnectionInvokeRequest): Promise<T> {
		return request<T>(this.env, "invoke", invoke as unknown as Record<string, unknown>);
	}

	async [LEGACY_CONNECTION_INVOKE_METHOD](invoke: ConnectionInvokeRequest): Promise<unknown> {
		return this.invoke(invoke);
	}
}
