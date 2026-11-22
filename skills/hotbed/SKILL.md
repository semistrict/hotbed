---
name: hotbed
description: Run builds, tests and dev servers on a hotbed cluster instead of this machine, in workspaces forked from a repo's already-built main, so a change compiles incrementally. Use when HOTBED_CONTROLLER is set or the hotbed CLI is installed and work needs compiling, testing or serving, especially Rust.
license: Apache-2.0
---

# hotbed

hotbed runs your commands on a cluster of Linux machines. For each repo it keeps a **warm base**:
the default branch, checked out and already built (dependencies, `target/`, toolchains, caches).
Your workspace is an instant copy-on-write fork of it, so the first build in it compiles only what
you changed.

## The loop

```sh
S=$(hotbed session start | jq -r .data.id)       # e.g. brave-otter
hotbed add $S ratel --sync .                     # a workspace of ratel that follows this checkout
hotbed run $S ratel -- cargo nextest run         # syncs your edits, runs, prints output, exits with its status
hotbed run $S ratel --detach -- cargo build --release
hotbed wait $S                                   # blocks until a command in the session changes state
hotbed session end $S
```

- **Everything happens in a session.** The session's name is the first argument of every command.
  `session start` picks a free adjective-noun name, or takes one you give it.
- **A session has one workspace per repo.** `hotbed add S REPO` makes it or returns the one it has.
  It is a fork of the repo's base at the branch's head. When the branch has moved or the repo's
  setup steps changed since the newest base, `add` first freshens a new base from the previous
  one: a pull and the steps that need to run (usually seconds to a minute; a repo's very first
  base builds from nothing). It waits for that, saying
  so on stderr; `--no-wait` returns at once with a `WORKSPACE_STATE_PREPARING` workspace, and
  commands run in it wait. Commands run in it at `/work/REPO`, and it keeps its state between
  commands.
- **Two ways to change code:**
  - `--sync DIR` ties the workspace to a local checkout. Every `hotbed run` from this machine first
    sends the checkout's state, uncommitted and untracked files included, ignored files not. Only
    files that differ are rewritten, so builds stay incremental. `hotbed sync S REPO` does it
    explicitly.
  - Without `--sync` the workspace is **remote-only**: change it with commands, as you would over
    ssh: `hotbed run S ratel -- git checkout -b fix`, `hotbed run S ratel -- sed -i ...`,
    `hotbed run S ratel -- bash -c 'cat > src/x.rs <<EOF ... EOF'`, `git commit`, `git push`.

## Missing a tool or dependency? Add a step

Never install things ad hoc in a workspace: the next workspace, and every other agent, would
lack them again. Make it a **setup step** of the repo instead:

```sh
hotbed step add $S ratel playwright -- 'pnpm exec playwright install chromium'
```

`step add` runs the script in your workspace, so you have what it installs at once. It runs it a
second time to check that it is idempotent, then saves it to the repo. Every base built after that
runs it, so every new workspace starts with it. A step is a bash script run with `-eo pipefail`
in the checkout, and it is like a database migration: a base runs it once, and again only when
its script changes. Add `--every-pull` for steps that must follow the code, such as builds and
installs from a lockfile.

- Install into the workspace, never into `$HOME` or the system, or forks won't have it. Programs go
  in `$HOTBED_BIN` (on PATH). Caches and data go in `$XDG_CACHE_HOME` and `$XDG_DATA_HOME`.
  `CARGO_HOME`, `RUSTUP_HOME`, `GOPATH` and npm's cache already point into the workspace.
- Make steps safe to repeat: check before installing, use `mkdir -p` and `--force`. A step that fails
  its second run is refused (`STEP_NOT_IDEMPOTENT`).
- To fix a step, add it again under the same name. `hotbed step ls REPO` shows each step and
  whether each node's newest base ran it. `hotbed step show REPO NAME` prints its script.
- If a step breaks a later base, new workspaces still get the last good base, with a warning
  (`reason`) that names the failed step. Fix the step and the next `add` retries.
- Steps run as an unprivileged user. OS packages (`apt`, `dnf`) are not steps: ask the person
  who runs the cluster.

## Keeping a session alive

A session nothing touches for **an hour** is abandoned: its commands stop and its workspaces are
deleted. Every hotbed call that names the session counts. While waiting on long work, call
`hotbed wait S` in a loop: each call returns within two minutes, with or without events, and keeps
the session alive.

## When the controller is out of reach

`run`, `sync`, `logs` and `stop` talk to the workspace's machine directly, so they keep working
on a workspace this machine used before, and the session stays alive while you do. `add`, `wait`
and `ps` need the controller: retry them on `CONTROLLER_UNREACHABLE`. What happened meanwhile
reaches `wait` once the controller is back.

## run

`hotbed run S REPO [flags] -- CMD...`

- Attached (the default): prints the command's output as it runs and exits with its status. Ctrl-C
  or killing `hotbed run` stops the command.
- `--detach`: prints the command (with its ID) as an envelope and returns at once. Use for dev
  servers (`--port 5173` puts its URL in the cockpit) and for work you will `wait` on.
- `--mem-low 2G` guarantees memory: the command queues until that fits. `--mem-high 8G` throttles
  and swaps past that. Nothing is killed for memory: when a node runs short, its newest command is
  **parked** (frozen, its memory swapped out) and resumes later. `wait` reports `parked` and
  `thawed`.
- No time limit. Commands run until they end or `hotbed stop S CMD`.
- Toolchains and caches (`RUSTUP_HOME`, `CARGO_HOME`, `GOMODCACHE`, `GOCACHE`, `XDG_*`) live
  inside the workspace, so the base's downloads and builds are already there.

## wait

`hotbed wait S` returns what happened to the session's commands since the last `wait` on this
machine:

```json
{"version":"1","ok":true,"command":"wait","data":{
  "events":[{"seq":"12","commandId":"k3m9x2qa","argv":"cargo nextest run","what":"exited",
             "state":"COMMAND_STATE_FAILED","exitCode":100,"workspaceId":"q7n2c4ze"}],
  "cursor":"12","live":1,"ended":false}}
```

`what` is `admitted`, `parked`, `thawed`, `exited` or `cancelled`. `live` counts commands not yet
ended. An empty `events` after two minutes means nothing happened yet; call it again. `ended: true`
means the session is over (`endReason` says why).

## Scheduled commands

Recurring work, such as a nightly measurement, runs by itself:

```sh
hotbed schedule add cladefs-nightly cladefs --cron '0 2 * * *' --node ubu1 \
  --env BOARD_URL=https://example/api/board -- scripts/nightly-host.sh
hotbed schedule show cladefs-nightly        # the command, the next run, its runs and their sessions
hotbed schedule run cladefs-nightly         # a run now; prints its session, for hotbed wait and logs
```

- **Each run is a session of its own**, `NAME-YYYYMMDD-HHMM`, with a fresh workspace of the repo
  at its branch's head, in which the command runs detached at `/work/REPO`. It takes `run`'s
  flags (`--env`, `--mem-low`, `--mem-high`, `--kind`). Nothing persists between runs except what
  the command keeps outside the workspace (pushed to git, or on the node: pin it with `--node`).
- **Its session's end.** A run is never abandoned while its command runs. One that succeeds ends
  its session. One that fails keeps it for the idle hour, so you can `hotbed run SESSION REPO --
  ...` in its workspace to look; its log stays (`hotbed logs SESSION COMMAND`).
- **Cron** has five fields in the controller's time zone (`CRON_TZ=Europe/Lisbon 0 2 * * *` for
  another). Runs never overlap: a slot that comes while the previous run is still going is
  `skipped`. A slot that passes while the controller is down is `missed`, not run late.
- `schedule add` with an existing name replaces it. `schedule pause`, `resume`, `rm`, `ls`.

## Other commands

| Command | Does |
|---|---|
| `hotbed ps S` | The session's commands, live and just ended |
| `hotbed logs S CMD [-f]` | A command's output; `-f` follows until it ends |
| `hotbed stop S CMD` | Stops a command |
| `hotbed repo ls` | Repos and their setup steps |
| `hotbed step ls REPO` | A repo's steps and which each node's newest base ran |
| `hotbed nodes` | The cluster's machines |

## Output

Every command except an attached `run` and `logs` prints one JSON envelope on stdout:

```json
{"version":"1","ok":false,"command":"add","error":{"code":"REPO_UNREACHABLE","message":"...","suggestion":"check the repo's URL (hotbed repo ls) and that this node can reach it; retry once it can","retryable":true}}
```

Act on `error.code`, never the message. `retryable: true` means the same command can succeed later
unchanged; otherwise `suggestion` says what to change. Exit status: 0 ok, 1 error, 2 bad arguments.
`run` exits with its command's status, and hotbed's own failures go to stderr.

Codes and what to do: `hotbed skills get hotbed/references/errors.md`. Setting up a cluster:
`hotbed skills get hotbed/references/setup.md`.
