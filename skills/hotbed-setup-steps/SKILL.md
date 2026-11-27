---
name: hotbed-setup-steps
description: Make a tool, dependency or pre-build part of every hotbed workspace with a project setup step, instead of installing it ad hoc, and inspect or rebuild a project's bases. Use when a command in a workspace lacks something, or a base fails a step.
license: MIT
---

# Missing a tool or dependency? Add a step

Never install things ad hoc in a workspace: the next workspace, and every other agent, would
lack them again. Make it a **setup step** of the project instead. By default, after all setup
steps have run, common dev commands (builds, tests, checks, linters, etc.) should run without
downloading or compiling anything:

```sh
hotbed step add playwright -- 'cd ratel && pnpm exec playwright install chromium'
```

`step add` runs the script in `/work` of your session's workspace, so you have what it installs at
once. It runs it a second time to check that it is idempotent, then saves it to the session's
project. Every base built after that runs it, so every new workspace starts with it. A step is a
bash script run with `-eo pipefail` in `/work`, beside every repo of the project (so `cd REPO`
first for one repo's build), and it is like a database migration: a base runs it once, and again
only when its script changes. Add `--every-pull` for steps that must follow the code, such as
builds and installs from a lockfile: they run again whenever any repo moved.

Bases run steps against the default branch's files. A branch that changes what a step reads (a
package list, a script it runs) must run that step's script in its own workspace first, or its
runs lack what the change adds.

- Install into the workspace, never into `$HOME` or the system, or forks won't have it. Programs go
  in `$HOTBED_BIN` (on PATH). Caches and data go in `$XDG_CACHE_HOME` and `$XDG_DATA_HOME`.
  `CARGO_HOME`, `RUSTUP_HOME`, `GOPATH` and npm's cache already point into the workspace.
  `$HOTBED_MEMORY_BYTES` carries the command's effective memory ceiling in bytes (the VM's RAM or the
  host work slice's limit), so memory-heavy build steps (e.g. `CARGO_BUILD_JOBS`) can scale parallelism
  to fit without OOM-killing on smaller nodes.
- Make steps safe to repeat: check before installing, use `mkdir -p` and `--force`. A step that fails
  its second run is refused (`STEP_NOT_IDEMPOTENT`).
- To fix a step, add it again under the same name. `hotbed step ls` (`-p PROJECT` for another
  project than the session's) shows each step and whether each node's newest base ran it. `hotbed
  step show NAME` prints its script. `hotbed project bases [PROJECT] [--node HOST] [--state
  building|warm|stale|failed]` lists every node's bases (newest generation first per node), their
  repo commits, freshen command IDs (for `hotbed logs CMD`), and any failed step. The cockpit's
  Projects page shows them too.
- `hotbed project rebuild PROJECT` has every node build its next base from nothing, for when
  incremental state has gone bad; `hotbed project hosts PROJECT HOST...` limits which nodes keep
  its bases.
- If a step breaks a later base, new workspaces still get the last good base, with a warning
  (`reason`) that names the failed step. Fix the step and the next run retries.
- Steps run chrooted as the build user with passwordless `sudo` to root: a step installs OS
  packages with `sudo apt-get install -y PKG`, and they are in every base after it. Commands and
  shells have the same `sudo`.
- In a vm workspace, a command profiles itself without `sudo`: `perf record -g -- CMD` (kernel
  stacks included) and `perf report` work as the workspace user, since the guest boots with
  `kernel.perf_event_paranoid=-1` and `kptr_restrict=0`.
