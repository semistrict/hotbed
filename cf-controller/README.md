# `cf-controller` — Hotbed Controller on Cloudflare Workers + Per-Organization SQLite Durable Objects

This directory is the `hotbed` controller running on **Cloudflare Workers** backed by **one SQLite-backed Durable Object (`ControllerDO`) per organization**.

## Architecture

- **One SQLite Durable Object (`ControllerDO`) per Organization + Drizzle ORM Auto-Migrations:**
  - When WorkOS AuthKit is configured (`WORKOS_CLIENT_ID`, `WORKOS_API_KEY`, `WORKOS_COOKIE_PASSWORD`), each WorkOS organization (`org_...`) routes to its own isolated SQLite `ControllerDO` (`org:<org_id>`, or `"default"` when `DEFAULT_ORG_ID` matches so an existing single-DO cluster migrates in place).
  - Multiple users per organization share that organization's nodes, projects, sessions, questions, memories, reviews, and schedules, while every organization's data, git credentials, and worker tokens stay physically isolated in its own DO SQLite database.
  - Schema is declared with Drizzle ORM in `src/db/schema.ts` and managed with Drizzle Kit (`drizzle.config.ts` with `driver: "durable-sqlite"`).
  - Generated SQL migrations in `drizzle/` are bundled as native Cloudflare Worker Text modules and **auto-run at Durable Object startup** via `await migrate(this.db, migrations)` inside `ctx.blockConcurrencyWhile(...)` before any request or alarm is handled.
  - All controller tables (including `kv_meta`, `worker_tokens`, `join_tokens`, `api_tokens`, `daemon_logs`) live in each org DO's embedded SQLite database.
- **Full ConnectRPC API parity:** Serves all 8 services (`ClusterService`, `SessionService`, `WorkspaceService`, `QuestionService`, `MemoryService`, `ReviewService`, `ScheduleService`, `GitAuthService`) using `@connectrpc/connect` + `@bufbuild/protobuf` (`protoc-gen-es`), including `hotbed.v1.ErrorInfo` details, `Hotbed-Build` headers, and `(param) = { local: true }` refusal.
- **Persistent Worker WebSocket Tunnel (`/ws/worker`):** Local Go workers behind NAT/Tailscale open an outbound WebSocket to `/ws/worker` (using Cloudflare's WebSocket Hibernation API with `"ping"`/`"pong"` auto-response). The DO multiplexes controller→worker `WorkspaceService` unary and server-streaming RPCs down this tunnel; direct client↔worker data-plane traffic (`Sync`, `RunCommand`, `FollowLog`, SSH) remains over Tailscale.
- **Background Maintenance via DO Alarms:** Replaces Go background goroutines with a Durable Object `alarm()` loop that reaps idle sessions, triggers due cron schedules, settles scheduled runs, polls GitHub Releases (`build-N` + `SHA256SUMS`), and prunes old telemetry.
- **Fail-Closed Authentication:**
  - **WorkOS AuthKit (multi-org, multi-user):** Browser OAuth + PKCE (`/auth/login` → `/auth/callback`, AES-256-GCM sealed `HttpOnly` cookie with transparent JWT refresh), CLI loopback PKCE & Device Flow (`hotbed login`, `/auth/cli/*`), local WorkOS JWKS JWT verification (`RS256`/`ES256`), and org-scoped tokens: worker join tokens (`hbj_<org_id>.<secret>`), node-bound worker tokens (`hbw_<org_id>.<secret>`, auto-minted on `ClusterService/Join`), and personal API tokens (`hbu_<org_id>.<secret>`). Defaults to `WORKOS_AUTH_PROVIDER=authkit` (supporting Google login and any other WorkOS-enabled providers/SSO with zero code changes).
  - `OWNER_TOKEN` / `JOIN_TOKEN` (single-tenant / dev): Bearer token for `hotbed` CLI (`HOTBED_TOKEN`) and Cockpit `/auth/login` (`HttpOnly; SameSite=Strict` HMAC cookie with same-origin CSRF checks).
  - Optional **Cloudflare Access** JWT verification (`Cf-Access-Jwt-Assertion`) when `CF_ACCESS_AUD` and `CF_ACCESS_TEAM_DOMAIN` are configured.
- **Self-Known Version & Direct GitHub Binary Updates:** CI stamps `HOTBED_BUILD_NUMBER`, `HOTBED_GIT_COMMIT`, and `HOTBED_BUILD_SHA256SUMS` into the Worker bundle at deploy time so the controller always knows its exact deployed version and target checksums. Heartbeats and `GetBuild` return the direct GitHub Release `download_url` + `sha256`, and workers running an older build fetch directly from GitHub, verify SHA-256, atomically replace their binary, and `execve` themselves (newer workers are never downgraded). `/builds/{build}/{asset}` remains available as a proxy fallback for older workers or private repos.

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
2. Configure secrets:
   ```sh
   # WorkOS AuthKit (multi-org / Google login + any other WorkOS providers):
   # In WorkOS Dashboard -> Redirects, add:
   #   https://<your-controller-domain>/auth/callback
   # And set Sign-out redirect URI to:
   #   https://<your-controller-domain>/auth/login
   cf workers secrets update --worker hotbed-controller --name WORKOS_CLIENT_ID --type secret_text
   cf workers secrets update --worker hotbed-controller --name WORKOS_API_KEY --type secret_text
   cf workers secrets update --worker hotbed-controller --name WORKOS_COOKIE_PASSWORD --type secret_text
   # Optional: map an existing single-DO cluster ("default") to your WorkOS org ID:
   #   cf workers secrets update --worker hotbed-controller --name DEFAULT_ORG_ID --type secret_text
   # Optional legacy/single-tenant secrets:
   cf workers secrets update --worker hotbed-controller --name OWNER_TOKEN --type secret_text
   cf workers secrets update --worker hotbed-controller --name JOIN_TOKEN --type secret_text
   # Optional (for private GitHub release downloads):
   cf workers secrets update --worker hotbed-controller --name GITHUB_TOKEN --type secret_text
   ```
3. (Optional) Enable Cloudflare Access for the Cockpit UI by setting `CF_ACCESS_AUD` and `CF_ACCESS_TEAM_DOMAIN` in `cloudflare.config.ts` and protecting the Worker's custom domain with an Access Application (bypassing `/hotbed.v1.*`, `/ws/worker`, `/builds/*`, `/healthz` for Bearer-authenticated CLI/workers).

## Regenerating Protobuf Bindings

`buf.gen.yaml` at the repo root generates TypeScript bindings directly into `cf-controller/src/gen/hotbed/v1/hotbed_pb.ts` whenever `make proto` / `buf generate` runs.
