import { SELF } from "cloudflare:test";
import { describe, expect, it } from "vite-plus/test";

const OWNER_AUTH = { Authorization: "Bearer test-owner-secret" };

function postCode(code: string, headers: Record<string, string> = {}): Promise<Response> {
	return SELF.fetch("https://controller.test/admin/signup-codes", {
		method: "POST",
		headers: { ...OWNER_AUTH, "Content-Type": "application/json", ...headers },
		body: JSON.stringify({ code }),
	});
}

describe("/admin/signup-codes", () => {
	it("refuses to write codes to an organization's controller, where sign-up never reads them", async () => {
		const res = await postCode("ORGSCOPED1", { "X-Hotbed-Org-Id": "org_signup_elsewhere" });
		expect(res.status).toBe(400);
		expect(await res.json()).toEqual({
			code: "INVALID_ARGS",
			message: "sign-up codes live in the default organization, not org_signup_elsewhere",
			suggestion: "omit X-Hotbed-Org-Id when managing sign-up codes",
		});
	});

	it("manages codes in the default controller", async () => {
		const res = await postCode("DEFAULT2026");
		expect(res.status).toBe(201);
		const list = await SELF.fetch("https://controller.test/admin/signup-codes", {
			headers: OWNER_AUTH,
		});
		expect(list.status).toBe(200);
		const body = (await list.json()) as { codes: Array<{ code: string }> };
		expect(body.codes.map((c) => c.code)).toContain("DEFAULT2026");
	});
});
