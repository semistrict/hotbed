---
name: hotbed
description: Run builds, tests and dev servers on a hotbed cluster instead of this machine, in workspaces forked from a repo's already-built main, so a change compiles incrementally. Use when the hotbed CLI is installed and work needs compiling, testing or serving, especially Rust.
license: Apache-2.0
---

# hotbed

hotbed runs your commands on a cluster of Linux machines. For each repo it keeps a **warm base**:
the default branch, checked out and already built (dependencies, `target/`, toolchains, caches).
Your workspace is an instant copy-on-write fork of it, so the first build in it compiles only what
you changed.

## The loop

```sh
S=$(hotbed session start | jq -r .data.id)       # e.g. brave-otter (bound to T3CODE_THREAD_ID or this git worktree in ~/.hotbed/sessions.json)
hotbed add --sync .                              # workspace of this repo following this checkout
hotbed run -- cargo nextest run                  # syncs your edits, runs, prints output, exits with its status
hotbed run --detach -- cargo build --release
hotbed wait                                      # blocks until a command in the session changes state
hotbed session end
```

- **A session works on a project.** A project is one or more repos, named by their canonical git
  remotes (`github.com/semistrict/hotbed`). `session start` picks the one project that holds this
  checkout's repo; a repo no project holds gets a project of its own name. When several projects
  hold it, or outside a checkout, name one: `-p PROJECT` or `HOTBED_PROJECT`. `hotbed project ls`
  lists them; `hotbed project add NAME --repo REMOTE...` makes one or adds repos to it (a repo
  another project holds needs `--shared`, since its checkouts' sessions would then need `-p`);
  `hotbed project rm-repo NAME --repo REMOTE` takes one out; `hotbed project show NAME` has every
  change, with who made it.
- **Everything happens in a session.** Session and repo are never positional arguments: override them with `-s`/`HOTBED_SESSION` and `-r`/`HOTBED_REPO`, or let hotbed default the session from `T3CODE_THREAD_ID` → current git worktree (stored in `~/.hotbed/sessions.json`) and infer the repo from the current git checkout. `hotbed session current` prints the active session.
  `session start` picks a free adjective-noun name, or takes one you give it. In a thread or
  worktree already bound to a live session it fails with `CONFLICT` naming that session: keep
  using it, end it first, or pass `--replace` to end it and bind the new one.
- **A session has one workspace per repo.** `hotbed add` (with `-r REPO` outside a checkout) makes it or returns the one it has.
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
    files that differ are rewritten, so builds stay incremental. `hotbed sync` does it
    explicitly.
  - Without `--sync` the workspace is **remote-only**: change it with commands, as you would over
    ssh: `hotbed run -r ratel -- git checkout -b fix`, `hotbed run -r ratel -- sed -i ...`,
    `hotbed run -r ratel -- bash -c 'cat > src/x.rs <<EOF ... EOF'`, `git commit`, `git push`.

## Missing a tool or dependency? Add a step

Never install things ad hoc in a workspace: the next workspace, and every other agent, would
lack them again. Make it a **setup step** of the repo instead:

```sh
hotbed step add -r ratel playwright -- 'pnpm exec playwright install chromium'
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
- To fix a step, add it again under the same name. `hotbed step ls` (or `-r REPO`) shows each step and
  whether each node's newest base ran it. `hotbed step show NAME` (or `-r REPO`) prints its script.
- If a step breaks a later base, new workspaces still get the last good base, with a warning
  (`reason`) that names the failed step. Fix the step and the next `add` retries.
- Steps run as an unprivileged user. OS packages (`apt`, `dnf`) are not steps: ask the person
  who runs the cluster.

## Asking the user: questions

When you need the user's decision, don't stop and wait: ask on the project's question list, carry
on with your best guess, and read the answer when it comes. A question takes exactly the JSON
AskUserQuestion takes; each of its questions becomes an entry of its own:

```sh
hotbed question add <<'EOF'
{"questions": [{"question": "Which store should sessions use?", "header": "Store", "multiSelect": false,
  "options": [{"label": "SQLite (Recommended)", "description": "One file, already used"},
              {"label": "Postgres", "description": "A server to run"}]}]}
EOF
hotbed question wait 12          # up to two minutes; or keep working and let hotbed wait tell you
                                 # (no IDs: any of this session's questions; --project-wide: any)
hotbed question outbox           # answers waiting for an agent
hotbed question ack 12           # you read it and acted on it: it is handled
```

- **One list per project, every session's.** `question ls` lists your project's questions, newest
  first (`--sort oldest|updated|rank`, `--state open|answered|dismissed|handled|deleted|all`,
  `-p PROJECT`, `--all-projects`). Read it before asking: another session may have asked already.
- **The user answers in the cockpit**, or dismisses (no answer will come). An answer waits in the
  outbox until an agent acknowledges it; any agent of the project may act on and ack an answer.
  An answer or dismissal is also an event of the asking session, so its `hotbed wait` wakes.
- **Any session may edit (`question edit ID`, while open) or delete (`question rm ID --reason`)
  any question** it finds wrong or no longer needed. Nothing is lost: `question show ID` has the
  whole history, each change with its session and time.
- A question records where you asked from (checkout, branch, commit, changed files) and, if the
  project holds your checkout's repo, that it is about that repo (`--repo REMOTE` names another).
- A session changes only its own project's questions (edit, rm, ack, answer, dismiss, move).
- Never answer a question yourself unless the user told you the answer: then pass it on with
  `question answer ID --pick LABEL [--other TEXT] [--notes TEXT] -s SESSION`, or `question dismiss
  ID --reason WHY`. `question move ID --first|--last|--before ID` reorders the user's list within
  the question's project. `question projects` counts every project's open and outbox questions.

## Reviews: asking a reviewer agent, and the user

A review is of an exact version: the commits it names never move. Ask for one in the checkout to
review, then give its number to a reviewer agent:

```sh
hotbed review request --title "Retry on lost leases" --instructions "look for races"
                                 # base: where HEAD left origin's default branch (--base REF, --whole)
                                 # head: HEAD, or a snapshot commit of uncommitted changes, pinned
hotbed review wait 7             # up to two minutes; or keep working and let hotbed wait tell you
hotbed review show 7             # findings, with locations, decisions and responses
hotbed review accept 7 1 3 --reason "fixing"
hotbed review dismiss 7 2 --reason "pre-existing; not this change"
hotbed review link 7             # the review's page in the cockpit: send it to the user
hotbed review wait 7 --responses # until the user (or anyone else) responds
hotbed review responses 7        # the responses since you last read them
```

The reviewer, in its own session:

```sh
hotbed review start 7            # claim it; review show 7 has base, head and instructions
git diff BASE HEAD               # the commits are in the requester's checkout (refs/hotbed/reviews/HEAD)
hotbed review findings 7 <<'EOF'
{"findings": [{"short_summary": "Lease renewal races expiry", "summary": "...", "failure_scenario": "...",
  "category": "correctness", "severity": "important", "verdict": "CONFIRMED", "file": "lease.go", "line": 88,
  "locations": [{"path": "renew.go", "start_line": 12, "end_line": 20, "excerpt": "..."}]}]}
EOF
hotbed review finish 7 --summary "One race, one nit."   # or: hotbed review fail 7 --reason WHY
```

- A finding is ReportFindings' JSON (severity `important`, `pre_existing` or `nit`; verdict
  `CONFIRMED`, `PLAUSIBLE` or none), with any number of `locations` besides, or none. Put the
  lines in `excerpt`: the cockpit shows them without the code.
- The user responds on the review's page, per finding, with a quick response (verify, fix if
  verified, won't fix), a comment, or both. A response, and the review's end, are events of the
  requesting session, so its `hotbed wait` wakes. Reply with `hotbed review respond 7 --finding 1
  --comment TEXT`; pass the user's word on with `--quick verify|fix-if-verified|wont-fix`.
- `hotbed review ls` lists the project's reviews; `review cancel` withdraws one; `review reopen`
  makes a decided finding open again. Nothing is removed: `review show` has every event.

## Keeping a session alive

A session nothing touches for **an hour** is abandoned: its commands stop and its workspaces are
deleted. Every hotbed call that names the session counts, and so does an attached `hotbed run` or
`hotbed logs -f` for as long as it streams, however many hours that is.

So never write a keep-alive loop. To wait on long work, either run it attached (in the
background if your harness lets you, and read its output when it exits) or `--detach` it and call
`hotbed wait`, which returns on its next event or after two minutes. A detached command nothing
follows or waits on is what the hour is for: an agent that went away leaves no dev server behind.

## When the controller is out of reach

`run`, `sync`, `logs` and `stop` talk to the workspace's machine directly, so they keep working
on a workspace this machine used before, and the session stays alive while you do. `add`, `wait`
and `ps` need the controller: retry them on `CONTROLLER_UNREACHABLE`. What happened meanwhile
reaches `wait` once the controller is back.

## run

`hotbed run [flags] -- CMD...` (session defaults from `HOTBED_SESSION` / `T3CODE_THREAD_ID` / git worktree; repo defaults from `HOTBED_REPO` / current git checkout; override with `-s` / `-r`)

- Attached (the default): prints the command's output as it runs and exits with its status. Ctrl-C
  or killing `hotbed run` stops the command.
- `--detach`: prints the command (with its ID) as an envelope and returns at once. Use for dev
  servers (`--port 5173` puts its URL in the cockpit) and for work you will `wait` on.
- `--mem-low 2G` guarantees memory: the command queues until that fits. `--mem-high 8G` throttles
  and swaps past that. Nothing is killed for memory: when a node runs short, its newest command is
  **parked** (frozen, its memory swapped out) and resumes later. `wait` reports `parked` and
  `thawed`.
- No time limit. Commands run until they end or `hotbed stop CMD`.
- `--bench` runs it as a **measurement**, for numbers you will compare (benchmarks, timings). It
  waits for any other measurement on the node to end and for running work to fit beside it, then
  runs alone on the node's measurement CPUs (`hotbed nodes`: `benchCpus`), which no other command
  uses meanwhile, with IRQs and kernel work moved off them and, where the node can, its own cache
  and memory bandwidth, the other cores' clocks capped and other work's disk IO capped. It never
  swaps and is never parked. If that isolation is lost mid-run it fails, saying so, whatever its
  status: rerun it. A node with no `benchCpus` refuses it: `hotbed add --node` one that has them,
  and compare only numbers from the same node.
- Toolchains and caches (`RUSTUP_HOME`, `CARGO_HOME`, `GOMODCACHE`, `GOCACHE`, `XDG_*`) live
  inside the workspace, so the base's downloads and builds are already there.

## wait

`hotbed wait` returns what happened to the session's commands since the last `wait` on this
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
hotbed schedule add cladefs-nightly -r cladefs --cron '0 2 * * *' --node ubu1 \
  --env BOARD_URL=https://example/api/board -- scripts/nightly-host.sh
hotbed schedule show cladefs-nightly        # the command, the next run, its runs and their sessions
hotbed schedule run cladefs-nightly         # a run now; prints its session, for hotbed wait and logs
```

- **Each run is a session of its own**, `NAME-YYYYMMDD-HHMM`, with a fresh workspace of the repo
  at its branch's head, in which the command runs detached at `/work/REPO`. It takes `run`'s
  flags (`--env`, `--mem-low`, `--mem-high`, `--kind`). Nothing persists between runs except what
  the command keeps outside the workspace (pushed to git, or on the node: pin it with `--node`).
- **Its session's end.** A run is never abandoned while its command runs. One that succeeds ends
  its session. One that fails keeps it for the idle hour, so you can `hotbed run -s SESSION -r REPO --
  ...` in its workspace to look; its log stays (`hotbed logs -s SESSION COMMAND`).
- **Cron** has five fields in the controller's time zone (`CRON_TZ=Europe/Lisbon 0 2 * * *` for
  another). Runs never overlap: a slot that comes while the previous run is still going is
  `skipped`. A slot that passes while the controller is down is `missed`, not run late.
- `schedule add` with an existing name replaces it. `schedule ls [-p PROJECT | --all-projects]`
  lists the active project's schedules by default. `schedule pause`, `resume`, `rm`.

## Other commands

| Command | Does |
|---|---|
| `hotbed session status` | Active session state, idle time, workspaces, and commands |
| `hotbed session current` | Active session and where it came from (`flag`, `env`, `thread`, `worktree`) |
| `hotbed session ls` | The project's live sessions (`--ended`, `--all-projects`) |
| `hotbed project ls` | Projects, their repos, live sessions, schedules, reviews and questions |
| `hotbed project show NAME` | A project and every change to it |
| `hotbed project rm-repo NAME --repo REMOTE` | Take a repo out of a project |
| `hotbed schedule add ... -p PROJECT` | A schedule's runs work on its project (default: the one holding its repo) |
| `hotbed schedule ls [-p PROJECT \| --all-projects]` | The project's schedules (`--all-projects`: every project's) |
| `hotbed ps [-p PROJECT \| --all]` | The session's commands, live and just ended (`-p`: a project's, `--all`: every session's) |
| `hotbed logs CMD [-f \| -n N]` | A command's output; `-f` follows until it ends, `-n N` its last N lines |
| `hotbed stop CMD` | Stops a command |
| `hotbed workspace ls [-p PROJECT \| --all]` | The session's workspaces (`-p`: a project's, `--all`: every session's) |
| `hotbed workspace show` | The session's workspace of a repo, as its worker has it now |
| `hotbed workspace files` | Its files, each `added`/`modified` against the base or unchanged |
| `hotbed workspace file PATH` | A file's text and the lines it changed against the base |
| `hotbed workspace release` | Gives the workspace up; the session goes on, and `hotbed add` gives it a new one |
| `hotbed repo add ID URL [-p PROJECT]` | Registers a repo, attaching it to `PROJECT` (or auto-creating a project for it) |
| `hotbed repo ls [-p PROJECT]` | Repos and their setup steps (`-p`: only a project's repos) |
| `hotbed step ls` | A repo's steps and which each node's newest base ran |
| `hotbed nodes` | The cluster's machines, and the build each runs |
| `hotbed snapshot` | Everything the cockpit shows, as JSON |
| `hotbed upgrade` | Install the controller's build over this hotbed (a CI build does it by itself) |

## Output

Every command except an attached `run` and `logs` prints one JSON envelope on stdout:

```json
{"version":"1","ok":false,"command":"add","error":{"code":"REPO_UNREACHABLE","message":"...","suggestion":"check the repo's URL (hotbed repo ls) and that this node can reach it; retry once it can","retryable":true}}
```

Act on `error.code`, never the message. `retryable: true` means the same command can succeed later
unchanged; otherwise `suggestion` says what to change. Exit status: 0 ok, 1 error, 2 bad arguments.
`run` exits with its command's status, and hotbed's own failures go to stderr.

Every session, workspace, command and node in the output carries `cockpitUrl`, its page in the
cockpit (the controller's UI): give it to the user when they want to watch or look into it. A
command's `url` is something else, the dev server it serves.

Codes and what to do: `hotbed skills get hotbed/references/errors.md`. Setting up a cluster:
`hotbed skills get hotbed/references/setup.md`.
