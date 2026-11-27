#!/usr/bin/env node
/**
 * Local workerd harness for Go integration tests (`cmd/hotbed`) and `cmd/dev-cockpit`.
 *
 * Boots a single Miniflare 5 instance running the built `cf-controller` bundle
 * with SQLite Durable Objects and prints a single JSON line on stdout:
 *   {"url":"http://127.0.0.1:<port>","token":"hbo_test_owner_token"}
 *
 * Exits cleanly on SIGTERM/SIGINT or when stdin closes.
 */
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { Miniflare } from "miniflare";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const rootDir = path.resolve(__dirname, "..");
const defaultBundleDir = path.join(rootDir, ".cloudflare/output/v1/workers/default/bundle");

function parseArgs(argv) {
	const out = {
		port: 0,
		owner: "ramon@",
		name: "ubu1",
		token: "hbo_test_owner_token",
		joinToken: "hbj_test_join_token",
		assets: "",
		persist: "",
		bundle: defaultBundleDir,
	};
	for (let i = 0; i < argv.length; i++) {
		const arg = argv[i];
		if (arg === "--bundle" && argv[i + 1]) out.bundle = path.resolve(argv[++i]);
		else if (arg === "--port" && argv[i + 1]) out.port = Number(argv[++i]);
		else if (arg === "--owner" && argv[i + 1]) out.owner = argv[++i];
		else if (arg === "--name" && argv[i + 1]) out.name = argv[++i];
		else if (arg === "--token" && argv[i + 1]) out.token = argv[++i];
		else if (arg === "--join-token" && argv[i + 1]) out.joinToken = argv[++i];
		else if (arg === "--assets" && argv[i + 1]) out.assets = path.resolve(argv[++i]);
		else if (arg === "--persist" && argv[i + 1]) out.persist = path.resolve(argv[++i]);
	}
	return out;
}

function collectModules(dir, base = dir, out = {}) {
	for (const ent of fs.readdirSync(dir, { withFileTypes: true })) {
		const full = path.join(dir, ent.name);
		if (ent.isDirectory()) {
			collectModules(full, base, out);
			continue;
		}
		if (ent.name.endsWith(".map")) continue;
		const rel = path.relative(base, full).split(path.sep).join("/");
		if (ent.name.endsWith(".sql") || ent.name.endsWith(".txt")) {
			out[rel] = { type: "text", contents: fs.readFileSync(full, "utf8") };
		} else if (ent.name.endsWith(".wasm")) {
			out[rel] = { type: "wasm", contents: new Uint8Array(fs.readFileSync(full)) };
		} else if (ent.name.endsWith(".js") || ent.name.endsWith(".mjs")) {
			out[rel] = { type: "esm", contents: fs.readFileSync(full, "utf8") };
		}
	}
	return out;
}

const opts = parseArgs(process.argv.slice(2));
const bundleDir = opts.bundle;
const entryPath = path.join(bundleDir, "index.js");

if (!fs.existsSync(entryPath)) {
	console.error(
		`cf-controller bundle missing at ${entryPath}; run \`pnpm -C cf-controller run build\` first`,
	);
	process.exit(1);
}

const modules = collectModules(bundleDir);

const workerEnv = {
	CONTROLLER: {
		type: "durable-object",
		worker: "hotbed-controller",
		exportName: "ControllerDO",
	},
	TENANT_REGISTRY: {
		type: "durable-object",
		worker: "hotbed-controller",
		exportName: "TenantRegistryDO",
	},
	COMMAND: {
		type: "durable-object",
		worker: "hotbed-controller",
		exportName: "CommandDO",
	},
	WORKER: {
		type: "durable-object",
		worker: "hotbed-controller",
		exportName: "WorkerDO",
	},
	ADMIN_AGENT: {
		type: "durable-object",
		worker: "hotbed-controller",
		exportName: "AdminAgentDO",
	},
	OWNER_TOKEN: { type: "text", value: opts.token },
	JOIN_TOKEN: { type: "text", value: opts.joinToken },
	CLUSTER_OWNER: { type: "text", value: opts.owner },
	CONTROLLER_NAME: { type: "text", value: opts.name },
	ALLOW_LOCAL_REPOS: { type: "text", value: "1" },
	TEST_HARNESS: { type: "text", value: "1" },
};

const workerConfig = {
	name: "hotbed-controller",
	compatibilityDate: "2026-11-19",
	compatibilityFlags: ["nodejs_compat"],
	manifest: {
		mainModule: "index.js",
		modulesRoot: bundleDir,
		modules,
	},
	exports: {
		ControllerDO: {
			type: "durable-object",
			storage: "sqlite",
		},
		TenantRegistryDO: {
			type: "durable-object",
			storage: "sqlite",
		},
		CommandDO: {
			type: "durable-object",
			storage: "sqlite",
		},
		WorkerDO: {
			type: "durable-object",
			storage: "sqlite",
		},
		AdminAgentDO: {
			type: "durable-object",
			storage: "sqlite",
		},
	},
	env: workerEnv,
};

if (opts.assets && fs.existsSync(opts.assets)) {
	workerEnv.ASSETS = { type: "assets" };
	workerConfig.assets = {
		directory: opts.assets,
		// The Worker is in front of its assets, as in production; without
		// hasUserWorker Miniflare's router refuses to hand it a request.
		runWorkerFirst: true,
		hasUserWorker: true,
	};
}

const mfOpts = {
	host: "127.0.0.1",
	port: opts.port,
	workers: [{ config: workerConfig }],
};
if (opts.persist) {
	mfOpts.resourcePersistencePath = opts.persist;
}

const mf = new Miniflare(mfOpts);
const url = await mf.ready;
process.stdout.write(
	`${JSON.stringify({ url: url.toString().replace(/\/+$/, ""), token: opts.token })}\n`,
);

let closing = false;
async function shutdown() {
	if (closing) return;
	closing = true;
	try {
		await mf.dispose();
	} catch {
		// ignore dispose errors on exit
	}
	process.exit(0);
}

process.on("SIGTERM", shutdown);
process.on("SIGINT", shutdown);
process.stdin.resume();
process.stdin.on("end", shutdown);
process.stdin.on("close", shutdown);
