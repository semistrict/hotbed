# The controller (Cloudflare Worker)

- Never share or cache an in-flight Durable Object RPC Promise across requests (module or instance state): workerd throws when one request awaits I/O another request started. Cache resolved values, or call per request, or serialize inside the Durable Object.
- Always call Durable Objects via typed Workers RPC methods rather than `.fetch()` (except WebSocket upgrades, which the Cloudflare Workers runtime requires to return `status: 101` from `fetch()`).
- Store per-command data (profiles, diagnostics, samples, cached output) in each command's own SQLite Durable Object (`CommandDO`, `env.COMMAND`), never in `ControllerDO`.
