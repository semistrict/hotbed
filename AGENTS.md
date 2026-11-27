# Working on hotbed

## Develop hotbed using hotbed

Use hotbed itself to build, test, and verify hotbed actively while working on this repo (especially Linux-specific code and tests):

```sh
hotbed session start                        # prints active memories and binds to T3CODE_THREAD_ID / worktree
hotbed run -- go test -race ./...           # the controller picks the node; --arch narrows it
hotbed session end
```

Do not pipe `hotbed session start` into `jq` or capture/swallow its stdout: it prints active memories directly and auto-binds the session so `-s` is not needed. Read `hotbed skills get hotbed` for the full CLI workflow. Session and repo are flags (`-s`/`HOTBED_SESSION`, `-r`/`HOTBED_REPO`) defaulted from `T3CODE_THREAD_ID` / the current git worktree and checkout, never positional arguments. A session idle for an hour is abandoned; any session call, and an attached `hotbed run` for as long as it runs, keeps it alive, so never wrap a run in a keep-alive loop. End it with `hotbed session end` when done.

## Put each lesson where it will be found

When you resolve a non-obvious bug or tooling quirk, or learn a standing preference, keep the lesson where the next agent will meet it: fix the cause (a script, a check, a clearer error); write a design decision as a comment beside the code it constrains, or here (a subdirectory's own `AGENTS.md`, such as `ui/` or `cf-controller/`, for rules about that part); put how to use hotbed in the built-in skill that covers it (`skills/`: `hotbed` for the core loop, `hotbed-questions`, `hotbed-memories`, …; a new area gets a skill of its own, linked from `.claude/skills/`). Save a hotbed memory (not a local markdown file) only for what has no such home: the user's preferences, facts about machines, and workarounds for open bugs, naming the bug's task. Every memory's summary costs every session's context.

```sh
hotbed memory ls -q WORD                     # one that already covers it? update it (memory edit ID, or add --supersede ID)
hotbed memory add --slug SHORT-SLUG --summary "When it applies (<= 280 chars)" --body "The memory" --rationale "Why / what triggered it"            # project memory
hotbed memory add --global --slug SHORT-SLUG --summary "When it applies" --rationale "Why / what triggered it" --body-file - <<'EOF'      # global memory
```

A memory is like a skill: every session start lists its summary, so make that one line saying when it applies, and put the memory itself in the body, which an agent reads with `hotbed memory get` once the summary applies. Keep each memory to one rule, so one can be retired without the others, and archive one once its bug is fixed or it moved into the repo.

## Every API call has a CLI command

Whatever the API (`proto/hotbed/v1/hotbed.proto`) can do, the `hotbed` CLI can do. Commands are made from the proto itself: an RPC annotated with `option (command) = {...}` is a CLI command and an MCP tool (`hotbed mcp`), its request's fields annotated with `(param)` its flags, arguments and parameters, and its comments their help (see "commands and tools" at the end of `hotbed.proto`; `internal/apicmd` runs them). Defaults go in typed expressions (`internal/apicmd/expr.go`); what an expression cannot say is a Go hook, which `protoc-gen-hotbed-hooks` puts in the generated `hotbedv1cli.Hooks` interface and `cmd/hotbed/api.go` implements, so a missing hook fails the build. Every flag is a request field: one the server acts on is best (it serves the CLI, MCP and the cockpit alike); one only the client acts on is `local` (`(param) = {local: true}`), which the client clears before the call and the server refuses set. When you add or change an RPC, annotate it in the same change (or, for what annotations cannot yet say, write the command by hand; for now a group of commands, such as `question`, is all generated or all hand-written), test that command in `cmd/hotbed`, and list it in the built-in skill that covers it (`skills/`). A full `go test ./cmd/hotbed` run fails on any RPC the tests' CLI never called (`cmd/hotbed/coverage_test.go`). The only exceptions are the calls workers make to the controller on their own (`nodeProtocol` there); a new one goes on that list only if no person or agent would ever make it.

## Commit only what compiled and passed its tests

Commit changes once they have compiled and their tests have passed (`go test -race ./...` via hotbed on Linux as well as local macOS tests for darwin code, plus `pnpm -C ui test && pnpm -C ui typecheck` when touching `ui/`): never commit a change that has not been compiled and tested.

## Work lands on main: no long-lived branches

A branch lives only as long as the work on it: once its tests pass, land it on `main` and push to `main` unless the user says otherwise. Pushing to `main` needs no question. Never leave finished work waiting on a branch, nor start work from a stale base: merge `main` in often so nothing drifts far from it.

After every push to `main`, CI builds the release and deploys the Cloudflare Worker controller automatically (`gh run list --workflow build --branch main`). Once the `build` workflow succeeds, run `hotbed upgrade` to install the new CLI on the PATH and check that `hotbed version` names the pushed commit. Never copy a local `go build` over it: a dev build (build 0) stops following the controller. Before ending your turn, watch the rollout finish: `hotbed nodes --json` shows the controller's build (`controllerVersion`) and each worker's (`resources.version`). Other agents push to `main` constantly, so rebase right before pushing. Never deploy a branch to the fleet's own nodes: one deployed by hand once rolled the controller back past `main`'s newer database schema and it crash-looped. Try a branch in a hotbed session instead.

## Don't wait on the user: decide, and ask in hotbed

Never ask the user anything directly in chat when you can make a reasonable call: make your best decision, act on it, and record non-obvious design choices or anything that genuinely needs the user's call (e.g., signing into an account or a fact with no reasonable guess) in hotbed with `hotbed question add`, marking the option you went ahead with `"implemented": true`, then carry on with whatever doesn't depend on it. Check `hotbed question ls` before asking, check `hotbed question outbox` before starting work, and acknowledge answers with `hotbed question ack ID` once acted on. Do not keep an `OPEN_QUESTIONS.md` file in the repo.

## Database migrations (Drizzle for controller, goose + sqlc for worker)

- **Controller (`cf-controller/`):** Single SQLite Durable Object (`ControllerDO`). Schema lives in `cf-controller/src/db/schema.ts`, migrations are generated via `pnpm -C cf-controller run db:generate` into `cf-controller/drizzle/` (formatted for `vp check` by the script), and auto-run at DO startup. `drizzle-kit generate` asks on a TTY whether a dropped and added table or column is a rename; without one, split such a change: remove the old, `db:generate`, add the new, `db:generate` (two migrations).
- **Worker (`internal/worker/db/`):** SQLite schema in `internal/worker/db/migrations/` is managed with `github.com/pressly/goose/v3` and runs automatically at startup inside `openDB` (`PRAGMA foreign_keys = ON` is enabled on every connection).
  - **DDL changes go in SQL migrations** (`NNNNN_name.sql` with `-- +goose Up` / `-- +goose Down`) so `sqlc` can parse the schema statically.
  - **Data backfills that need Go logic** (protobuf JSON, canonical remotes, etc.) go in numbered Go migrations registered with `goose.WithGoMigrations` in `migrations.go`.
  - **Typed queries live in `queries/*.sql` and compile via `make sqlc` (`sqlc generate`).** Never edit files in `db/gen/` by hand. With SQLite in `sqlc`, do not mix `sqlc.arg(...)` (`?N`) with bare `?` or `sqlc.slice(...)` in the same statement (`TestSQLcGeneratedQueriesHaveSafeSQLitePlaceholders` enforces this).
- **Wire formats & Go↔TS schemas live in protobuf:** define every RPC payload, JSON schema, and data format shared between Go and TypeScript in `proto/hotbed/v1/hotbed.proto` — never hand-maintain duplicate Go/TS JSON structs, and don't keep legacy wire-format compatibility shims when a cleaner RPC or protobuf change calls for it.

## No silent fallbacks to a costlier path: fail hard

When the right/fast path cannot work, fail hard with a clear error and actionable suggestion rather than silently falling back to a slower or broken path. Check preconditions up front where possible.

## Never write a test around a bug

When a test only passes by steering clear of what the code under test gets wrong, file the bug as a task first and name the task in a comment at any temporary workaround. Never weaken a check or assertion to make a test pass.

## Every bug exposes a blind spot and questions the structure

For every bug found:
1. **Name the blind spot** (why existing tests missed it) and **add a test for the class**, not just the instance; verify it fails with the bug reintroduced.
2. **Look around it** for the same pattern elsewhere.
3. **Question the structure:** could types, ownership, or interfaces make this class of bug impossible? Refactor small structural issues cleanly before fixing, and record what you concluded in the task notes.

## No pre-AGI mechanisms

Don't hard-code behavior a model would decide better in context: canned decorations, keyword or emoji triggers, or heuristics that guess what a message or command means and rewrite it. Deliver what the caller wrote and give agents clean primitives and structured state.

## Dead ends: check them before you try, add to them when you stop

`dead-ends/` holds approaches that were tried and given up on (`dead-ends/<short-kebab-name>.md` with Tried / Idea / Result / Why it's dead / Reopen if). Check it before designing, add to it when abandoning an approach, and update rather than delete if a "Reopen if" condition later comes true.
