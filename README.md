# hotbed

> [!CAUTION]
> ## WARNING: ALPHA SOFTWARE — REGULARLY CRASHES MY ENTIRE OS — TOTALLY UNSECURED
>
> 1. **This is ALPHA software that regularly crashes my entire OS.** Hotbed aggressively drives cgroups, memory pressure, swap/zram, copy-on-write storage, FUSE, VMs, and eBPF profiling. Expect kernel hangs, OOM lockups, and data loss on machines you run it on.
> 2. **This software is TOTALLY UNSECURED — ONLY run it for your own trusted code on machines you exclusively control.**
>    - Host-mode workspaces have **no security boundary** from the host and grant **passwordless `sudo` to real root**.
>    - Workers listen on no network address. They connect outbound over authenticated HTTPS/WSS tunnels to the Cloudflare Worker controller, which checks WorkOS sign-in or an org-scoped token on every call.
>    - **Never** run it on shared networks, multi-user machines, or for untrusted code.

Run AI coding agents' builds, tests, and dev servers on a fleet of machines in copy-on-write workspaces forked from already-built project bases.

- **Controller & cockpit:** Cloudflare Worker + SQLite Durable Object ([cf-controller/](cf-controller/))
- **Workers & CLI:** Go binary ([cmd/hotbed/](cmd/hotbed/)) — run `hotbed --help` or `hotbed skills get hotbed` for current usage

## License

MIT — see [LICENSE](LICENSE).
