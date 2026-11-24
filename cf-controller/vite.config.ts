import { cloudflare } from "@cloudflare/vite-plugin";
import { defineConfig } from "vite-plus";
import { readBuildNumber, readBuildSha256Sums, readGitCommit } from "./scripts/build-meta.mjs";

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
	publicDir: "../ui/dist",
	define: {
		__HOTBED_BUILD_NUMBER__: JSON.stringify(readBuildNumber()),
		__HOTBED_GIT_COMMIT__: JSON.stringify(readGitCommit()),
		__HOTBED_BUILD_SHA256SUMS__: JSON.stringify(readBuildSha256Sums()),
	},
	plugins: [sqlRawPlugin(), cloudflare()],
	fmt: { useTabs: true },
	lint: {
		options: { typeAware: true, typeCheck: true },
	},
});
