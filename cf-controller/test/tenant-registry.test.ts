import { env } from "cloudflare:test";
import { describe, expect, it } from "vite-plus/test";
import type { ControllerEnv } from "../src/controller-do.ts";

describe("TenantRegistryDO global cladefs tenant assignment", () => {
	it("reserves tenant 0 for the default org regardless of initialization order and allocates unique positive IDs for all other orgs", async () => {
		const testEnv = env as unknown as ControllerEnv;
		const reg = testEnv.TENANT_REGISTRY!.getByName("test-order-independence");

		// A new non-default org waking up first gets tenant 1, never 0.
		expect(await reg.assignTenant("org_new_first", { isDefault: false })).toBe("1");
		expect(await reg.assignTenant("org_new_second", { isDefault: false })).toBe("2");

		// The existing/default org waking up later still claims tenant 0 (zero data loss).
		expect(await reg.assignTenant("org_existing_active", { isDefault: true })).toBe("0");

		// Subsequent calls are idempotent for every org.
		expect(await reg.assignTenant("org_existing_active", { isDefault: true })).toBe("0");
		expect(await reg.assignTenant("org_existing_active", { isDefault: false })).toBe("0");
		expect(await reg.assignTenant("org_new_first", { isDefault: false })).toBe("1");

		// A later non-default org continues monotonically from 3.
		expect(await reg.assignTenant("org_new_third", { isDefault: false })).toBe("3");

		// If DEFAULT_ORG_ID is ever changed to a different real org, it must NOT collide on tenant 0.
		expect(await reg.assignTenant("org_different_default", { isDefault: true })).toBe("4");

		const all = await reg.listTenants();
		expect(all.map((a) => [a.orgKey, a.tenantId])).toEqual([
			["org_existing_active", "0"],
			["org_new_first", "1"],
			["org_new_second", "2"],
			["org_new_third", "3"],
			["org_different_default", "4"],
		]);
	});

	it("promotes a legacy 'default' placeholder on tenant 0 to the real default org ID in place", async () => {
		const testEnv = env as unknown as ControllerEnv;
		const reg = testEnv.TENANT_REGISTRY!.getByName("test-default-promotion");

		// Single-tenant cluster initializes under "default" -> tenant 0.
		expect(await reg.assignTenant("default", { isDefault: true })).toBe("0");
		expect(await reg.assignTenant("org_other", { isDefault: false })).toBe("1");

		// Later WorkOS DEFAULT_ORG_ID binds the default cluster to a real org ID -> keeps tenant 0.
		expect(await reg.assignTenant("org_real_default", { isDefault: true })).toBe("0");
		expect(await reg.getTenant("org_real_default")).toBe("0");
		expect(await reg.getTenant("default")).toBeNull();

		const all = await reg.listTenants();
		expect(all.map((a) => [a.orgKey, a.tenantId])).toEqual([
			["org_real_default", "0"],
			["org_other", "1"],
		]);
	});
});
