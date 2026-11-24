# `cf-controller` — Hotbed Controller on Cloudflare Workers + Single SQLite Durable Object

This directory ports the `hotbed` controller (`internal/controller/`) to run on **Cloudflare Workers** backed by a **single SQLite-backed Durable Object** (`ControllerDO`), alongside the existing Go controller so you can deploy and cut over safely.

## Architecture

- **Single SQLite Durable Object (`ControllerDO`) + Drizzle ORM Auto-Migrations:**
  - Schema is declared with Drizzle ORM in `src/db/schema.ts` and managed with Drizzle Kit (`drizzle.config.ts` with `driver: "durable-sqlite"`).
  - Generated SQL migrations in `drizzle/` are bundled as native Cloudflare Worker Text modules and **auto-run at Durable Object startup** via `await migrate(this.db, migrations)` inside `ctx.blockConcurrencyWhile(...)` before any request or alarm is handled.
  - All 20 `hotbed.db` tables (plus `kv_meta`, `worker_tokens`, `daemon_logs`, `controller_migration`) match the Go controller's schema and protojson documents 1-for-1.
- **Full ConnectRPC API parity:** Serves all 7 services (`ClusterService`, `SessionService`, `WorkspaceService`, `QuestionService`, `ReviewService`, `ScheduleService`, `GitAuthService`) using `@connectrpc/connect` + `@bufbuild/protobuf` (`protoc-gen-es`), including `hotbed.v1.ErrorInfo` details, `Hotbed-Build` headers, and `(param) = { local: true }` refusal.
- **Persistent Worker WebSocket Tunnel (`/ws/worker`):** Local Go workers behind NAT/Tailscale open an outbound WebSocket to `/ws/worker` (using Cloudflare's WebSocket Hibernation API with `"ping"`/`"pong"` auto-response). The DO multiplexes controller→worker `WorkspaceService` unary and server-streaming RPCs down this tunnel; direct client↔worker data-plane traffic (`Sync`, `RunCommand`, `FollowLog`, SSH) remains over Tailscale.
- **Background Maintenance via DO Alarms:** Replaces Go background goroutines with a Durable Object `alarm()` loop that reaps idle sessions, triggers due cron schedules, settles scheduled runs, polls GitHub Releases (`build-N` + `SHA256SUMS`), and prunes old telemetry.
- **Fail-Closed Authentication:**
  - `OWNER_TOKEN`: Bearer token for `hotbed` CLI (`HOTBED_TOKEN`) and Cockpit `/auth/login` (`HttpOnly; SameSite=Strict` HMAC cookie with same-origin CSRF checks).
  - `JOIN_TOKEN` + per-worker tokens (`POST /admin/worker-token`): Scoped worker auth for `Join`, `Heartbeat`, `PushEvents`, `/ws/worker`, and `/builds/*`.
  - Optional **Cloudflare Access** JWT verification (`Cf-Access-Jwt-Assertion`) when `CF_ACCESS_AUD` and `CF_ACCESS_TEAM_DOMAIN` are configured.
- **Binary Distribution (`/builds/{build}/{asset}`):** Streams release binaries from GitHub Releases with an optional R2 cache (`BUILDS_BUCKET`, separate from `hotbed-cladefs`).

## Local Development & Testing

```sh
cd cf-controller
cp .dev.vars.example .dev.vars   # set OWNER_TOKEN / JOIN_TOKEN for local dev
pnpm install
pnpm run db:generate             # regenerate drizzle/ migrations after schema.ts edits
pnpm test                        # runs real workerd integration tests via @cloudflare/vitest-plugin
pnpm run check                   # cf workers types + oxlint + typecheck + format check
pnpm run build                   # cf build
pnpm run dev                     # cf dev (local workerd)
```

## Deploy to Cloudflare

1. Deploy the Worker + SQLite Durable Object with `cf`:
   ```sh
   cd cf-controller
   cf deploy
   ```
2. Configure required secrets:
   ```sh
   npx wrangler secret put OWNER_TOKEN --name hotbed-controller
   npx wrangler secret put JOIN_TOKEN --name hotbed-controller
   # Optional (for private GitHub release downloads):
   npx wrangler secret put GITHUB_TOKEN --name hotbed-controller
   ```
3. (Optional) Enable Cloudflare Access for the Cockpit UI by setting `CF_ACCESS_AUD` and `CF_ACCESS_TEAM_DOMAIN` in `cloudflare.config.ts` and protecting the Worker's custom domain with an Access Application (bypassing `/hotbed.v1.*`, `/ws/worker`, `/builds/*`, `/healthz` for Bearer-authenticated CLI/workers).

## Live Migration from an Existing Controller (e.g. `ubu1`)

Once workers and your CLI are upgraded to a build containing this change:

1. Deploy `cf-controller` and set `OWNER_TOKEN` and `JOIN_TOKEN`.
2. Trigger a one-command live migration against your existing Go controller:
   ```sh
   hotbed admin migrate "https://hotbed-controller.<your-subdomain>.workers.dev" \
     --token "$OWNER_TOKEN"
   ```
   What happens automatically:
   - The source controller exports its `clusterId` and all durable tables from `hotbed.db` and POSTs them to `POST /admin/import` on `cf-controller`.
   - `cf-controller` imports the snapshot atomically, preserves `clusterId`, and mints a dedicated `hbw_...` per-worker bearer token for every active node.
   - The source controller persists **migrated state** (`controller_migration` table) and refuses new mutating client RPCs with a pointer to the new URL.
   - On each worker's next `Heartbeat` or `PushEvents` (within seconds — or whenever a sleeping worker wakes up later), the source controller replies with `migrated_to` carrying the target URL, `controller_node_id`, and that worker's minted token.
   - Each worker atomically rewrites its own `hotbed.json` and restarts into `cf-controller` (opening `/ws/worker` and keeping all warm ZFS/btrfs bases and systemd-managed workloads intact).
3. Point your CLI at the new controller:
   ```sh
   export HOTBED_CONTROLLER="https://hotbed-controller.<your-subdomain>.workers.dev"
   export HOTBED_TOKEN="$OWNER_TOKEN"
   hotbed nodes
   ```

## Regenerating Protobuf Bindings

`buf.gen.yaml` at the repo root generates TypeScript bindings directly into `cf-controller/src/gen/hotbed/v1/hotbed_pb.ts` whenever `make proto` / `buf generate` runs.
