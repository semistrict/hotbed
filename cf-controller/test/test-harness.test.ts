import { env, runDurableObjectAlarm, runInDurableObject } from "cloudflare:test";
import { describe, expect, it, vi } from "vite-plus/test";
import type { ControllerDO, ControllerEnv } from "../src/controller-do.ts";
import { handleTestHarness, TestOverrides, type TestHarnessHost } from "../src/test-harness.ts";

describe("test harness", () => {
	it("pauses the maintenance alarm when a test asks, so a moved clock never races it", async () => {
		const overrides = new TestOverrides();
		const host = {
			overrides,
			runtime: () => ({ ctrl: { clusterId: "c1" } }),
		} as unknown as TestHarnessHost;
		const post = (body: unknown) =>
			handleTestHarness(
				host,
				new Request("https://controller.test/__test/config", {
					method: "POST",
					body: JSON.stringify(body),
				}),
				new URL("https://controller.test/__test/config"),
			);
		expect(await (await post({ maintenanceAlarm: false })).json()).toEqual({
			ok: true,
			clusterId: "c1",
		});
		expect(overrides.maintenancePaused).toBe(true);

		const stub = (env as unknown as ControllerEnv).CONTROLLER.getByName("harness-alarm");
		const ran = await runInDurableObject(stub, async (instance: ControllerDO, state) => {
			const internals = instance as unknown as { test: TestOverrides };
			internals.test.maintenancePaused = true;
			await state.storage.setAlarm(Date.now());
			return vi.spyOn(instance, "runMaintenance");
		});
		await runDurableObjectAlarm(stub);
		expect(ran).not.toHaveBeenCalled();

		await post({ maintenanceAlarm: true });
		expect(overrides.maintenancePaused).toBe(false);
	});
});
