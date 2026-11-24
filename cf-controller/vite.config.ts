import { cloudflare } from "@cloudflare/vite-plugin";
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
	publicDir: "../ui/dist",
	plugins: [sqlRawPlugin(), cloudflare()],
	fmt: { useTabs: true },
	lint: {
		options: { typeAware: true, typeCheck: true },
	},
});
