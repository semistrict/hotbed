# Working on hotbed

## Develop hotbed using hotbed

Use hotbed itself to build, test, and verify hotbed actively while working on this repo (especially Linux-specific code and tests):

```sh
S=$(hotbed session start | jq -r .data.id)   # e.g. selected-pug (bound to T3CODE_THREAD_ID / worktree in ~/.hotbed/sessions.json)
hotbed run -- go test -race ./...           # the controller picks the node; --arch/--node narrow it
hotbed session end
```

Read `hotbed skills get hotbed` for the full CLI workflow. Session and repo are flags (`-s`/`HOTBED_SESSION`, `-r`/`HOTBED_REPO`) defaulted from `T3CODE_THREAD_ID` / the current git worktree and checkout, never positional arguments. A session idle for an hour is abandoned; any session call, and an attached `hotbed run` for as long as it runs, keeps it alive, so never wrap a run in a keep-alive loop. End it with `hotbed session end` when done.

## Every API call has a CLI command

Whatever the API (`proto/hotbed/v1/hotbed.proto`) can do, the `hotbed` CLI can do. Commands are made from the proto itself: an RPC annotated with `option (command) = {...}` is a CLI command and an MCP tool (`hotbed mcp`), its request's fields annotated with `(param)` its flags, arguments and parameters, and its comments their help (see "commands and tools" at the end of `hotbed.proto`; `internal/apicmd` runs them). Defaults go in CEL; what CEL cannot say is a Go hook, which `protoc-gen-hotbed-hooks` puts in the generated `hotbedv1cli.Hooks` interface and `cmd/hotbed/api.go` implements, so a missing hook fails the build. Every flag is a request field: one the server acts on is best (it serves the CLI, MCP and the cockpit alike); one only the client acts on is `local` (`(param) = {local: true}`), which the client clears before the call and the server refuses set. When you add or change an RPC, annotate it in the same change (or, for what annotations cannot yet say, write the command by hand; for now a group of commands, such as `question`, is all generated or all hand-written), test that command in `cmd/hotbed`, and list it in `skills/hotbed/SKILL.md`. A full `go test ./cmd/hotbed` run fails on any RPC the tests' CLI never called (`cmd/hotbed/coverage_test.go`). The only exceptions are the calls workers make to the controller on their own (`nodeProtocol` there); a new one goes on that list only if no person or agent would ever make it.

## Commit only what compiled and passed its tests

Commit changes once they have compiled and their tests have passed (`go test -race ./...` via hotbed on Linux as well as local macOS tests for darwin code, plus `pnpm -C ui test && pnpm -C ui typecheck` when touching `ui/`): never commit a change that has not been compiled and tested.

## Work lands on main: no long-lived branches

A branch lives only as long as the work on it: once its tests pass, land it on `main` and push to `main` unless the user says otherwise. Pushing to `main` needs no question. Never leave finished work waiting on a branch, nor start work from a stale base: merge `main` in often so nothing drifts far from it.

After every push to `main`, install its build on the PATH: once the push's `build` workflow run succeeds (`gh run list --workflow build --branch main`), run `hotbed admin upgrade && hotbed upgrade` and check that `hotbed version` names the pushed commit. `admin upgrade` returns once the controller answers on the new build; the controller also restarts into it by itself within two minutes of CI, and while it does the commands fail `CONTROLLER_UNREACHABLE` (retryable) for a few seconds: run them again. Never copy a local `go build` over it: a dev build (build 0) stops following the controller.

## Don't wait on the user: decide, and ask in hotbed

Never ask the user anything directly in chat when you can make a reasonable call: make your best decision, act on it, and record non-obvious design choices or anything that genuinely needs the user's call (e.g., signing into an account or a fact with no reasonable guess) in hotbed with `hotbed question add`, marking the option you went ahead with `"implemented": true`, then carry on with whatever doesn't depend on it. Check `hotbed question ls` before asking, check `hotbed question outbox` before starting work, and acknowledge answers with `hotbed question ack ID` once acted on. Do not keep an `OPEN_QUESTIONS.md` file in the repo.

## Database migrations (goose) and typed queries (sqlc)

SQLite schemas for the controller (`internal/controller/db/migrations/`) and worker (`internal/worker/db/migrations/`) are managed with `github.com/pressly/goose/v3` and run automatically at startup inside `OpenStore` / `openDB` (`PRAGMA foreign_keys = ON` is enabled on every connection).

- **DDL changes go in SQL migrations** (`NNNNN_name.sql` with `-- +goose Up` / `-- +goose Down`) so `sqlc` can parse the schema statically.
- **Data backfills that need Go logic** (protobuf JSON, canonical remotes, etc.) go in numbered Go migrations registered with `goose.WithGoMigrations` in `migrations.go`.
- **Typed queries live in `queries/*.sql` and compile via `make sqlc` (`sqlc generate`).** Never edit files in `db/gen/` by hand. With SQLite in `sqlc`, do not mix `sqlc.arg(...)` (`?N`) with bare `?` or `sqlc.slice(...)` in the same statement (`TestSQLcGeneratedQueriesHaveSafeSQLitePlaceholders` enforces this).
- **Wire formats stay clean:** don't keep legacy wire-format compatibility shims when a cleaner RPC or protobuf change calls for it.

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
