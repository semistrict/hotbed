import { cloudflareTest } from "@cloudflare/vitest-plugin";
import { defineConfig } from "vite-plus";

function sqlRawPlugin() {
	return {
		name: "drizzle-sql-raw",
		transform(code: string, id: string) {
			if (id.endsWith(".sql")) {
				return {
					code: `export default ${JSON.stringify(code)};`,
					map: null,
				};
			}
			return null;
		},
	};
}

export default defineConfig({
	define: {
		__HOTBED_BUILD_NUMBER__: JSON.stringify(0),
		__HOTBED_GIT_COMMIT__: JSON.stringify(""),
		__HOTBED_BUILD_SHA256SUMS__: JSON.stringify(""),
	},
	plugins: [
		sqlRawPlugin(),
		cloudflareTest({
			config: true,
			miniflare: {
				bindings: {
					OWNER_TOKEN: "test-owner-secret",
					JOIN_TOKEN: "test-join-secret",
					GITHUB_TOKEN: "",
					GC_TOKEN: "",
					GC_WORKER_URL: "",
					WORKOS_CLIENT_ID: "",
					WORKOS_API_KEY: "",
					WORKOS_COOKIE_PASSWORD: "",
					CLUSTER_OWNER: "ramon",
					UPDATES_REPO: "",
					ALLOW_LOCAL_REPOS: "1",
				},
			},
		}),
	],
});
