import { describe, expect, it } from "vitest";
import { env } from "cloudflare:test";

describe("cloudflare bindings", () => {
	it("exposes starter env vars in worker tests", () => {
		expect(env.VALUE_FROM_CLOUDFLARE).toBe("Starter template running on Cloudflare");
	});
});

/*
 * Route + DO integration tests are intentionally commented out by default
 * because EXAMPLE_DO is disabled in wrangler.jsonc until the user opts in.
 *
 * import {
 *   loader as contactsLoader,
 *   action as contactsAction,
 * } from "../../app/routes/contacts";
 *
 * // Example structure:
 * // - call route action with a POST Request
 * // - assert data in env.EXAMPLE_DO
 */
