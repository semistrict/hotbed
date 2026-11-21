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
- **A session has one workspace per repo.** `hotbed add S REPO` makes it (a fork of the newest warm
  base) or returns the one it has. Commands run in it at `/work/REPO`, and it keeps its state
  between commands.
- **Two ways to change code:**
  - `--sync DIR` ties the workspace to a local checkout. Every `hotbed run` from this machine first
    sends the checkout's state, uncommitted and untracked files included, ignored files not. Only
    files that differ are rewritten, so builds stay incremental. `hotbed sync S REPO` does it
    explicitly.
  - Without `--sync` the workspace is **remote-only**: change it with commands, as you would over
    ssh: `hotbed run S ratel -- git checkout -b fix`, `hotbed run S ratel -- sed -i ...`,
    `hotbed run S ratel -- bash -c 'cat > src/x.rs <<EOF ... EOF'`, `git commit`, `git push`.

## Keeping a session alive

A session nothing touches for **an hour** is abandoned: its commands stop and its workspaces are
deleted. Every hotbed call that names the session counts. While waiting on long work, call
`hotbed wait S` in a loop: each call returns within two minutes, with or without events, and keeps
the session alive.

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

## Other commands

| Command | Does |
|---|---|
| `hotbed ps S` | The session's commands, live and just ended |
| `hotbed logs S CMD [-f]` | A command's output; `-f` follows until it ends |
| `hotbed stop S CMD` | Stops a command |
| `hotbed repo ls` | Repos with warm bases, and their freshen commands |
| `hotbed nodes` | The cluster's machines |

## Output

Every command except an attached `run` and `logs` prints one JSON envelope on stdout:

```json
{"version":"1","ok":false,"command":"add","error":{"code":"NO_WARM_BASE","message":"...","suggestion":"wait: g1 on asahimbp is 60% freshened","retryable":true}}
```

Act on `error.code`, never the message. `retryable: true` means the same command can succeed later
unchanged; otherwise `suggestion` says what to change. Exit status: 0 ok, 1 error, 2 bad arguments.
`run` exits with its command's status, and hotbed's own failures go to stderr.

Codes and what to do: `hotbed skills get hotbed/references/errors.md`. Setting up a cluster:
`hotbed skills get hotbed/references/setup.md`.
