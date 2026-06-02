import { DurableObject } from "cloudflare:workers";

/**
 * Tracks chat sessions for an anonymous user (keyed by cookie value).
 *
 * Each browser gets a unique `chat-owner` cookie, and this DO stores
 * the list of chat session IDs + titles for that owner. The actual
 * messages live in the per-session Chat DOs — this is just the index.
 *
 * Pattern: "index DO that references other DOs" — one of the most
 * common Cloudflare patterns for organizing related Durable Objects.
 */

export type ChatSessionRow = {
  id: string;
  title: string;
  created_at: string;
  updated_at: string;
};

export class ChatSessionsDO extends DurableObject<Env> {
  private sql: SqlStorage;

  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    this.sql = ctx.storage.sql;

    this.sql.exec(`
      CREATE TABLE IF NOT EXISTS sessions (
        id TEXT PRIMARY KEY,
        title TEXT NOT NULL DEFAULT 'New Chat',
        created_at TEXT DEFAULT CURRENT_TIMESTAMP,
        updated_at TEXT DEFAULT CURRENT_TIMESTAMP
      )
    `);
  }

  /** List all sessions, most recently updated first. */
  async listSessions(): Promise<ChatSessionRow[]> {
    return this.sql
      .exec<ChatSessionRow>(
        "SELECT id, title, created_at, updated_at FROM sessions ORDER BY updated_at DESC"
      )
      .toArray();
  }

  /** Create a session if it doesn't exist, or touch its updated_at. */
  async ensureSession(id: string): Promise<ChatSessionRow> {
    const existing = this.sql
      .exec<ChatSessionRow>("SELECT * FROM sessions WHERE id = ?", id)
      .toArray();

    if (existing.length === 0) {
      this.sql.exec("INSERT INTO sessions (id) VALUES (?)", id);
    } else {
      this.sql.exec(
        "UPDATE sessions SET updated_at = CURRENT_TIMESTAMP WHERE id = ?",
        id
      );
    }

    return this.sql
      .exec<ChatSessionRow>("SELECT * FROM sessions WHERE id = ?", id)
      .one();
  }

  /** Update the title of a session (e.g. from the first user message). */
  async updateTitle(id: string, title: string): Promise<void> {
    this.sql.exec(
      "UPDATE sessions SET title = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
      title,
      id
    );
  }

  /** Delete a session from the index. Returns true if a row was actually removed. */
  async deleteSession(id: string): Promise<boolean> {
    this.sql.exec("DELETE FROM sessions WHERE id = ?", id);
    return this.sql.exec("SELECT changes() AS c").one<{ c: number }>().c > 0;
  }
}
