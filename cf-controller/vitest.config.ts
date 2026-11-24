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
	plugins: [
		sqlRawPlugin(),
		cloudflareTest({
			config: true,
			miniflare: {
				bindings: {
					OWNER_TOKEN: "test-owner-secret",
					JOIN_TOKEN: "test-join-secret",
					GITHUB_TOKEN: "",
					CLUSTER_OWNER: "ramon",
					UPDATES_REPO: "",
					ALLOW_LOCAL_REPOS: "1",
				},
			},
		}),
	],
});
