import { DurableObject } from "cloudflare:workers";
import type { Contact, CreateContactInput } from "../app/schemas/contact";

/**
 * Example Durable Object with RPC methods and SQLite storage.
 *
 * Key patterns:
 * - Call methods directly on stub: `stub.listContacts()` (not fetch!)
 * - Use `this.ctx.storage.sql` for SQLite queries
 * - Create tables in constructor (runs once per DO instance)
 */
export class ExampleDO extends DurableObject<Env> {
  private sql: SqlStorage;

  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    this.sql = ctx.storage.sql;

    // Create tables on first access (idempotent)
    this.sql.exec(`
      CREATE TABLE IF NOT EXISTS contacts (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        name TEXT NOT NULL,
        email TEXT NOT NULL,
        created_at TEXT DEFAULT CURRENT_TIMESTAMP
      )
    `);
  }

  /**
   * RPC method - call directly from route loaders/actions:
   *   const stub = context.cloudflare.env.EXAMPLE_DO.get(id);
   *   const contacts = await stub.listContacts();
   */
  async listContacts(): Promise<Contact[]> {
    const results = this.sql.exec<Contact>(
      "SELECT id, name, email, created_at FROM contacts ORDER BY created_at DESC"
    );
    return results.toArray();
  }

  /**
   * RPC method for creating a contact.
   * Input is validated with Zod in the route action before calling this.
   */
  async createContact(input: CreateContactInput): Promise<Contact> {
    const result = this.sql.exec<Contact>(
      "INSERT INTO contacts (name, email) VALUES (?, ?) RETURNING *",
      input.name,
      input.email
    );
    return result.one();
  }

  /**
   * RPC method for deleting a contact.
   */
  async deleteContact(id: number): Promise<void> {
    this.sql.exec("DELETE FROM contacts WHERE id = ?", id);
  }

  /**
   * Test helper for executing raw SQL. Only use in tests.
   * Useful for setting up test data or verifying database state.
   */
  _testExecSql(sql: string, ...params: unknown[]): unknown[] {
    return this.sql.exec(sql, ...params).toArray();
  }
}
