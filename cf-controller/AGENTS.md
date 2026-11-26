# The controller (Cloudflare Worker)

- Never share or cache an in-flight Durable Object RPC Promise across requests (module or instance state): workerd throws when one request awaits I/O another request started. Cache resolved values, or call per request, or serialize inside the Durable Object.
