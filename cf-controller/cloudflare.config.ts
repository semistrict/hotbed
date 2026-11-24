import { bindings, defineConfig, exports } from "cf/config";
import * as entrypoint from "./src/index.ts" with { type: "cf-worker" };

export default defineConfig({
	worker: {
		name: "hotbed-controller",
		compatibilityDate: "2026-11-19",
		compatibilityFlags: ["nodejs_compat"],
		entrypoint,
		exports: {
			ControllerDO: exports.durableObject({ storage: "sqlite" }),
		},
		env: {
			CONTROLLER: bindings.durableObject({
				worker: "hotbed-controller",
				exportName: "ControllerDO",
			}),
			ASSETS: bindings.assets(),
			OWNER_TOKEN: bindings.secret(),
			JOIN_TOKEN: bindings.secret(),
			GITHUB_TOKEN: bindings.secret(),
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
		},
	},
});
