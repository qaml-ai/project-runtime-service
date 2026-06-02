import { WorkerEntrypoint } from "cloudflare:workers";

const DEFAULT_TIMEOUT_MS = 30_000;
const DEFAULT_MAX_RESPONSE_BYTES = 8 * 1024 * 1024;

interface LocalDataProxyEnv {
	DATA_PROXY_URL?: string;
	DATA_PROXY_MAX_RESPONSE_BYTES?: string;
}

export interface DataProxyServiceError {
	message: string;
	status?: number;
	code?: string;
	number?: number;
}

export type DataProxyServiceResult<T> =
	| { ok: true; data: T }
	| { ok: false; error: DataProxyServiceError };

export type SqlQueryMode = "read" | "modify";

export interface MssqlQueryRequest {
	mode: SqlQueryMode;
	server: string;
	port?: number;
	user: string;
	password: string;
	database?: string;
	query: string;
	params?: Record<string, unknown>;
	encrypt?: boolean;
	trustServerCertificate?: boolean;
}

export interface SqlQueryRequest {
	mode: SqlQueryMode;
	host: string;
	port?: number;
	user: string;
	password: string;
	database?: string;
	query: string;
	params?: unknown[];
}

export interface PostgresQueryRequest extends SqlQueryRequest {
	sslmode?: string;
}

export interface MysqlQueryRequest extends SqlQueryRequest {
	tls?: string;
	charset?: string;
}

export interface SqlQueryResponse {
	recordset?: Record<string, unknown>[];
	rowsAffected?: number[];
	error?: string;
	code?: string;
	number?: number;
}

function toServiceError(error: unknown, fallbackMessage: string): DataProxyServiceError {
	return {
		message: error instanceof Error ? error.message : fallbackMessage,
		status:
			typeof (error as { status?: unknown })?.status === "number"
				? (error as { status: number }).status
				: undefined,
		code:
			typeof (error as { code?: unknown })?.code === "string"
				? (error as { code: string }).code
				: undefined,
		number:
			typeof (error as { number?: unknown })?.number === "number"
				? (error as { number: number }).number
				: undefined,
	};
}

function resolveMaxResponseBytes(env: LocalDataProxyEnv): number {
	const raw = (env.DATA_PROXY_MAX_RESPONSE_BYTES ?? "").trim();
	if (!raw) return DEFAULT_MAX_RESPONSE_BYTES;

	const parsed = Number.parseInt(raw, 10);
	if (!Number.isFinite(parsed) || parsed <= 0) {
		return DEFAULT_MAX_RESPONSE_BYTES;
	}
	return parsed;
}

async function readTextWithLimit(response: Response, maxBytes: number): Promise<string> {
	const contentLengthRaw = response.headers.get("content-length") ?? "";
	if (contentLengthRaw) {
		const contentLength = Number.parseInt(contentLengthRaw, 10);
		if (Number.isFinite(contentLength) && contentLength > maxBytes) {
			throw new Error(`Local data proxy response too large (${contentLength} bytes > limit ${maxBytes} bytes)`);
		}
	}

	if (!response.body) {
		return response.text();
	}

	const reader = response.body.getReader();
	const decoder = new TextDecoder();
	let totalBytes = 0;
	let text = "";

	while (true) {
		const { done, value } = await reader.read();
		if (done) break;

		totalBytes += value.byteLength;
		if (totalBytes > maxBytes) {
			await reader.cancel("response exceeds max bytes");
			throw new Error(`Local data proxy response too large (${totalBytes} bytes > limit ${maxBytes} bytes)`);
		}

		text += decoder.decode(value, { stream: true });
	}

	text += decoder.decode();
	return text;
}

async function readJson(response: Response, maxBytes: number): Promise<unknown> {
	const body = await readTextWithLimit(response, maxBytes);
	if (!body.trim()) return {};
	try {
		return JSON.parse(body);
	} catch {
		return { error: `Local data proxy returned non-JSON response (${response.status})` };
	}
}

/**
 * Local DATA_PROXY shim used by the starter template.
 * Deploy pipeline rewrites this binding to the platform's internal DataProxyService.
 */
export class LocalDataProxyService extends WorkerEntrypoint<LocalDataProxyEnv> {
	private baseUrl(): string {
		const raw = (this.env.DATA_PROXY_URL ?? "").trim();
		if (!raw) {
			throw new Error("DATA_PROXY_URL is not configured for local DATA_PROXY service");
		}
		return raw.replace(/\/+$/, "");
	}

	private async request<T>(
		path: string,
		body: unknown,
		fallbackMessage: string
	): Promise<DataProxyServiceResult<T>> {
		const controller = new AbortController();
		const timeout = setTimeout(() => controller.abort(), DEFAULT_TIMEOUT_MS);

		try {
			const response = await fetch(`${this.baseUrl()}${path}`, {
				method: "POST",
				headers: { "Content-Type": "application/json" },
				body: JSON.stringify(body),
				signal: controller.signal,
			});

			const payload = (await readJson(response, resolveMaxResponseBytes(this.env))) as Record<string, unknown>;
			if (!response.ok) {
				return {
					ok: false,
					error: {
						message:
							typeof payload.error === "string"
								? payload.error
								: `Local data proxy request failed (${response.status})`,
						status: response.status,
						code: typeof payload.code === "string" ? payload.code : undefined,
						number: typeof payload.number === "number" ? payload.number : undefined,
					},
				};
			}

			return { ok: true, data: payload as T };
		} catch (error) {
			if (error instanceof Error && error.name === "AbortError") {
				return {
					ok: false,
					error: { message: `Local data proxy request timed out after ${DEFAULT_TIMEOUT_MS}ms`, status: 504 },
				};
			}
			return { ok: false, error: toServiceError(error, fallbackMessage) };
		} finally {
			clearTimeout(timeout);
		}
	}

	async mssqlQuery(request: MssqlQueryRequest): Promise<DataProxyServiceResult<SqlQueryResponse>> {
		return this.request<SqlQueryResponse>("/mssql/query", request, "MSSQL query failed");
	}

	async postgresQuery(request: PostgresQueryRequest): Promise<DataProxyServiceResult<SqlQueryResponse>> {
		return this.request<SqlQueryResponse>("/postgres/query", request, "Postgres query failed");
	}

	async mysqlQuery(request: MysqlQueryRequest): Promise<DataProxyServiceResult<SqlQueryResponse>> {
		return this.request<SqlQueryResponse>("/mysql/query", request, "MySQL query failed");
	}
}
