# Decisions

## Always-on profiling embeds OpenTelemetry eBPF Profiler directly in the Linux worker (2026-11-21)

- **Embedded vs external daemon:** Rather than requiring every worker host to install and manage a separate `otelcol-ebpf-profiler` / Pyroscope stack, the Linux worker embeds `go.opentelemetry.io/ebpf-profiler` directly (`CGO_ENABLED=0`, using its embedded `tracer.ebpf.amd64` / `tracer.ebpf.arm64` objects) at 20 Hz, always on. Processes are attributed to hotbed commands via a `MetaEnricher` reading `/proc/<pid>/cgroup` (`hotbed-<id>.service`) and `HOTBED_COMMAND` env fallback. Native ELF/Go/Rust/C++ frames are symbolized locally via Go's `debug/elf`, `debug/gosym`, and `demangle`, while kernel (`kallsyms`) and interpreted runtimes (Python, V8, JVM, Ruby, PHP, Perl, .NET, BEAM, LuaJIT) are symbolized by the profiler itself.
- **Storage & retention:** Live profiles accumulate in bounded memory on the worker; on command completion a compact gzip-compressed protobuf snapshot (`CommandProfile`) is saved to `<stateDir>/profiles/<id>.pb.gz` and shipped on `CommandChange.profile` to the controller's `command_profiles` SQLite table (same 30-day retention as `history`), making profiles queryable after commands end or workers go offline.
- **Alternatives & undoing:** An alternative is exporting OTLP Profiles (`pprofile.Profiles`) to an external OTel Collector + Pyroscope backend. To switch, replace `hotbedReporter` in `internal/profiler/otel_linux.go` with `ebpf-profiler`'s OTLP reporter or add a dual-export consumer alongside the in-memory `Store`.

## Client commands take their controller from worker init, never a flag (2026-11-21)

`--controller` is gone from every client command. They call the controller `hotbed worker init`
recorded in `~/.hotbed` (`vm/config.json` on macOS, `setup.json` on Linux). `HOTBED_CONTROLLER`
stays, and wins, for machines that are no worker (laptops, ubu1 when its worker was set up with
`hotbed join`, scripts): without it they could not call the cluster at all. To drop it, delete
the env read in `localController` (cmd/hotbed/main.go) and give such machines another way to
record a controller.

## The event stream keeps old workers working (2026-11-21, branch event-stream)

AGENTS.md says no migrations or compatibility shims. The brief for the event stream asked for a
migration "with no flag day: the cluster runs agents' work right now", so this change keeps
three: the controller still takes `Report` from workers that do not push events, an upgraded
worker delivers its old outbox once through `Report`, and schema migration 3 copies each node's
last report into its view. Without them, a deploy would lose the session events and history of
commands that end during it, and old workers would drop out until upgraded.

Undoing it, once every worker pushes events: delete `Server.Report`, `Store.Ingest`, the worker's
`legacyReport`/`drain`/outbox table, and the `report` copy in migration 3; reserve the proto
fields.

## Updates: CI builds, the controller follows, everything follows the controller (board 6318)

- **The controller reads releases with the user's own GitHub token,** from their gh login on ubu1
  (`gh auth token`), kept root-only in `/var/lib/hotbed/controller/github-token` by the host's
  setup step (155-hotbed.sh). The repo is private, so the controller needs a token; this one
  already lives on that machine, so nothing new is exposed there. It has more scope than reading
  releases needs. The alternative is a fine-grained token with read access to this repo's
  contents. To switch, write it to that file; the setup step would then need to stop rewriting
  the file.
- **A dev build on the controller is replaced within two minutes** by CI's newest. Branches are
  tried in hotbed sessions (AGENTS.md), never deployed to the fleet's own nodes, so nothing holds
  a build back. To undo, add a hold to the controller's `updates`, for example a pinned build.
- **Workers run exactly the controller's build,** newer or older. A controller moved back to an
  older build takes every worker back with it.
- **Clients follow only when they run a CI build.** A dev build (someone building hotbed) is left
  alone; `hotbed upgrade` installs the controller's build into it on demand.
- **No automatic rollback.** A build that crashes at startup stays broken until a newer one, or a
  release installed by hand with the fleet skill's `hotbed/deploy.sh`. CI's full test run gates
  every release.
- **CI keeps the newest 20 releases.**

# Open questions
