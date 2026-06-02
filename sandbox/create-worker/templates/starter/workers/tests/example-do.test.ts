import { describe, it, expect } from "vitest";

/**
 * Example test file showing how to test Durable Objects with vitest-pool-workers.
 *
 * Key patterns:
 * - Import `env` from "cloudflare:test" to get DO bindings
 * - Use `env.BINDING_NAME.idFromName()` and `env.BINDING_NAME.get()` to get stubs
 * - Call RPC methods directly on the stub
 * - Each test gets isolated storage automatically
 *
 * KNOWN LIMITATION: Tests that expect DO methods to throw exceptions may cause
 * "Failed to pop isolated storage stack frame" errors with vitest-pool-workers.
 * Avoid testing exception scenarios directly; instead test the error handling
 * in your route actions where you call the DO methods.
 */

describe("workers test setup", () => {
	it("runs a basic smoke test", () => {
		expect(1 + 1).toBe(2);
	});
});

/*
 * Example DO tests are intentionally commented out by default because
 * EXAMPLE_DO is disabled in wrangler.jsonc until the user opts in.
 *
 * import { env } from "cloudflare:test";
 *
 * describe("ExampleDO", () => {
 *   function getStub() {
 *     const id = env.EXAMPLE_DO.idFromName("test");
 *     return env.EXAMPLE_DO.get(id);
 *   }
 *
 *   it("creates and reads contacts", async () => {
 *     const stub = getStub();
 *     await stub.createContact({ name: "Ada", email: "ada@example.com" });
 *     const contacts = await stub.listContacts();
 *     expect(contacts).toHaveLength(1);
 *   });
 * });
 */
