import { bindings, defineConfig, exports } from "cf/config";
import * as entrypoint from "./src/index.ts" with { type: "cf-worker" };

export default defineConfig({
	worker: {
		name: "hotbed-controller",
		domains: ["hotbed.semistrict.com"],
		workersDev: true,
		compatibilityDate: "2026-11-19",
		compatibilityFlags: ["nodejs_compat"],
		entrypoint,
		exports: {
			ControllerDO: exports.durableObject({ storage: "sqlite" }),
			TenantRegistryDO: exports.durableObject({ storage: "sqlite" }),
		},
		env: {
			CONTROLLER: bindings.durableObject({
				worker: "hotbed-controller",
				exportName: "ControllerDO",
			}),
			TENANT_REGISTRY: bindings.durableObject({
				worker: "hotbed-controller",
				exportName: "TenantRegistryDO",
			}),
			ASSETS: bindings.assets(),
			OWNER_TOKEN: bindings.secret(),
			JOIN_TOKEN: bindings.secret(),
			GITHUB_TOKEN: bindings.secret(),
			GC_TOKEN: bindings.secret(),
			GC_WORKER_URL: bindings.text("https://cladefs-gc.ramon3525.workers.dev"),
			// WorkOS Google-only auth (multi-organization, multi-user auth; one ControllerDO per org):
			//   Configure Worker secrets (injected onto env automatically):
			//     WORKOS_CLIENT_ID, WORKOS_API_KEY, WORKOS_COOKIE_PASSWORD (optional), DEFAULT_ORG_ID (optional)
			//   GoogleOAuth redirects straight to Google; non-Google authentication methods are rejected.
			WORKOS_AUTH_PROVIDER: bindings.text("GoogleOAuth"),
			// Initial sign-up code seeded once into the default ControllerDO for new accounts after Google auth.
			INITIAL_SIGNUP_CODE: bindings.text("LC2026051"),
			CLUSTER_OWNER: bindings.text("ramon@echophase.com"),
			CONTROLLER_NAME: bindings.text("cloudflare-do"),
			UPDATES_REPO: bindings.text("semistrict/hotbed"),
			// Optional Cloudflare Access vars:
			//   CF_ACCESS_AUD, CF_ACCESS_TEAM_DOMAIN
			// Optional R2 bucket binding for caching /builds/{build}/{asset} (separate from hotbed-cladefs):
			//   BUILDS_BUCKET: bindings.r2({ bucketName: "hotbed-builds" }),
		},
		assets: {
			runWorkerFirst: true,
		},
		observability: {
			enabled: true,
			logs: {
				enabled: true,
			},
			traces: {
				enabled: true,
			},
		},
	},
});
