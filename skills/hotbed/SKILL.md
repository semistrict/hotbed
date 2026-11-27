---
name: hotbed
description: Run builds, tests and dev servers on a hotbed cluster instead of this machine, in workspaces forked from a repo's already-built main, so a change compiles incrementally. Use when the hotbed CLI is installed and work needs compiling, testing or serving, especially Rust.
license: MIT
---

# hotbed

hotbed runs your commands on a cluster of Linux machines. For each repo it keeps a **warm base**:
the default branch, checked out and already built (dependencies, `target/`, toolchains, caches).
Your workspace is an instant copy-on-write fork of it, so the first build in it compiles only what
you changed.

This skill is the core: sessions, runs, output and waiting. hotbed's other built-in skills cover
the rest; a session start lists them, and `hotbed skills get NAME` reads one:

| Skill | When |
|---|---|
| `hotbed-setup-steps` | A workspace lacks a tool, dependency or pre-build; a base fails a step |
| `hotbed-questions` | You need the user's decision, or `poll` brings an answer |
| `hotbed-reviews` | A change needs review, or you were given a review number |
| `hotbed-memories` | A listed memory's summary applies, or a lesson has no better home |
| `hotbed-skills` | Adding, installing or reading Agent Skills stored in hotbed |
| `hotbed-board` | Coordinating with other agents through threads or a task queue |
| `hotbed-schedules` | Work should recur on a cron schedule |

## The loop

```sh
hotbed session start                             # lists skills and memories (one line each) & binds to T3CODE_THREAD_ID / worktree
hotbed run -- go vet ./...                       # quick work: placed on a node, syncs your edits there, runs, prints output, exits with its status
                                                 # (125 if still running after 2 min: left running, detached; see run)
hotbed run --detach --mem 8G -- cargo nextest run  # long work: say what it needs; prints the command's ID and returns
hotbed poll                                      # what happened since the last poll: commands ended, answers, reviews (up to 2 min)
hotbed output CMD --grep 'FAIL|panicked' -C 3    # read its output, kept whole: never filter a run's output as it runs
hotbed session end                               # ends session and reminds you to save any lessons learned
```

Do not pipe `hotbed session start` into `jq` or swallow its stdout: it lists every skill (hotbed's
built-in ones first, then the project's and global ones) and every memory, each as one line saying
when it applies, and auto-binds the session so `-s` is not needed. Read a skill with
`hotbed skills get NAME` and a memory with `hotbed memory get SLUG` once its line applies.

- **A session works on a project.** A project is one or more repos, named by their canonical git
  remotes (`github.com/semistrict/hotbed`). `session start` picks the one project that holds this
  checkout's repo; a repo no project holds gets a project of its own name. When several projects
  hold it, or outside a checkout, name one: `-p PROJECT` or `HOTBED_PROJECT`. `hotbed project ls`
  lists them; `hotbed project add NAME --repo REMOTE...` makes one or adds repos to it (a repo
  another project holds needs `--shared`, since its checkouts' sessions would then need `-p`);
  `hotbed project rm-repo NAME --repo REMOTE` takes one out; `hotbed project show NAME` has every
  change, with who made it.
- **Everything happens in a session.** Session and repo are never positional arguments: override them with `-s`/`HOTBED_SESSION` and `-r`/`HOTBED_REPO`, or let hotbed default the session from `T3CODE_THREAD_ID` → current git worktree (stored in `~/.hotbed/sessions.json`) and infer the repo from the current git checkout. `hotbed session current` prints the active session.
  `session start` picks a free adjective-noun name, or takes one you give it. When `T3CODE_THREAD_ID`
  is set (or `--t3code-thread-id ID` is passed), the controller also persists that T3 Code thread ID
  on the session so `hotbed session search [THREAD_ID]`, `hotbed session ls --t3code-thread-id ID`,
  and `hotbed session status --t3code-thread-id ID` can find live and ended sessions by thread from
  any machine. It also keeps the thread's link, read from `$T3CODE_CLI_PATH thread show` (a warning,
  not a failure, when T3 Code cannot give it), so the cockpit's session pages open the thread. In a thread or
  worktree already bound to a live session it fails with `CONFLICT` naming that session: keep
  using it, end it first, or pass `--replace` to end it and bind the new one.
  `session start NAME --resume` takes up session NAME again: live again if it ended (with no
  workspaces), as it is if not. It keeps its project (and associates `T3CODE_THREAD_ID` if none was
  set yet).
- **The controller places every run.** Each `hotbed run` goes to the node it picks by load: it
  keeps a session where it already has a workspace of the repo unless that node is clearly busier
  than another, and otherwise prefers a node with a warm base. Leave the choice to it, so work
  spreads over the cluster. Narrow it only when the run needs something: `--arch amd64|arm64` for
  an architecture, `--runtime vm|cladefs` for a runtime, or `--bench`, `--mem-low`, `--vm-cpus`,
  and `--vm-mem` for resource requirements. stderr says where it ran.
- **A workspace per node, of the whole project, made as needed (auto-captured bases).** A workspace
  holds every repo of the session's project, each checked out at `/work/REPO`, side by side. The
  first run of a session on a node forks the project's newest warm base immediately without waiting
  for a background freshen, and `hotbed sync` fixes up tracked files (preserving mtimes of unchanged
  files for incremental builds, even when starting from a bare OS base image or another worktree's
  captured commit). Routine runs stay strictly local-only on the worker's SSD (zero R2 uploads).
  When a project's shared R2 base is missing or stale (default 30m with a newer upstream merge-base,
  or after `hotbed project rebuild`), the controller designates one session's workspace to
  auto-capture: after its next successful command exits and its VM stops, the worker versions and
  uploads that disk as the project's new shared base in the background, then swaps the workspace to
  a local-only ephemeral fork for all subsequent commands. Pass `--no-capture` on `hotbed run` to
  opt out, `--capture-base` to force capturing after a command succeeds (e.g. after installing a
  system package), or `--fresh` to ignore the cached project base and start from the bare OS base
  image. A run starts in `/work/REPO` of the repo it is for, or in `/work` when it is for none
  (outside any checkout, no `-r`). Each workspace keeps its state between the commands run in it.
  On the vm runtime each command runs in a fresh VM booted on the workspace's disk and stopped when
  it ends, so a second command of the session on that node waits until the first ends.
- **Your checkout is the truth.** Run from a checkout of the repo and each run first sends its
  state to the workspace it runs in, uncommitted and untracked files included, ignored files not.
  Only files that differ are rewritten, so builds stay incremental. Anything a command writes
  stays on its node: a run on another node sees the base plus your checkout, not it. `hotbed
  sync` sends the checkout to every workspace the session has. A run of a repo the session's
  project does not hold is refused: add its remote with `hotbed project add PROJECT --repo REMOTE`.
  In the workspace your checkout's state is a `hotbed sync` commit at `HEAD`, over the base's
  commit: to compare with the base, use `git show HEAD~1:PATH` or a SHA, never `HEAD:PATH`.
- **Without a checkout** (`-r REPO` elsewhere, or `--no-sync`) a run changes only what its
  workspace holds, so it stays in the session's workspace, as over ssh: `hotbed run -r ratel --
  git checkout -b fix`, `hotbed run -r ratel -- sed -i ...`, `git commit`, `git push`. If that
  node is offline it fails retryably rather than start over; `--arch` naming another
  architecture starts a fresh workspace there.

## Problems with hotbed itself: issues

When hotbed itself fails you (a bug, a missing capability, an error that misleads you), say so,
once: list first, and +1 one already reported rather than report it again. Its votes are how the
user sees what costs agents most.

```sh
hotbed issue ls -q "wait queued"                # open issues whose title or body hold the words
hotbed issue upvote 12 -m "same, on vm1"         # you hit it too: each session counts once
hotbed issue report "run --wait counts time queued" -m "what you did, what happened, what you expected; the command and error"
hotbed issue show 12                             # it, and everyone who +1'd it
```

Work around it meanwhile, and say so in your reply. The user archives an issue once it is dealt
with (`hotbed issue archive ID -m WHY`, `hotbed issue reopen ID`): an archived one takes no more
+1s, so if it is back, report it again naming it. The cockpit's Issues page lists them.

## Keeping a session alive

A session nothing touches for **an hour** is abandoned: its commands stop and its workspaces are
deleted. Every hotbed call that names the session counts, and so does an attached `hotbed run` or
`hotbed logs -f` for as long as it waits on its command.

So never write a keep-alive loop. `hotbed poll` counts too: an agent polling for its work keeps its
session. A detached command nothing polls or waits on is what the hour is for: an agent that went
away leaves no dev server behind.

## hotbed as MCP tools & skills

`hotbed mcp` serves hotbed's commands as MCP tools over stdin and stdout (`question add` is the tool
`question_add`, its flags and arguments the tool's parameters), for an agent to start as a local
MCP server, e.g. `claude mcp add hotbed -- hotbed mcp`. The server is a session: it takes the one
`HOTBED_SESSION` or this T3 thread or git worktree names (resumed if it ended), or starts one bound
here, so `hotbed` commands run here use it too. It keeps the session alive while it runs, ends it
when it exits, and the next server started here resumes it. Its tools never take a session.

It also advertises the **MCP Skills Extension (`io.modelcontextprotocol/skills`, SEP-2640)** with
`directoryRead: true`, implementing `skills/list`, `skills/get`, `resources/directory/read`, and
`resources/read` (plus standard `resources/list` for every skill's `SKILL.md`), and puts what
`session start` lists (skills, hotbed's built-in ones first, and memories) in
`InitializeResult.instructions`. A built-in skill is `skill://builtin/NAME/SKILL.md`.

## Waiting: poll

Everything that happens in your session later reaches you through **one call, `hotbed poll`**:
your commands admitted, parked, paused or ended, your workspaces ready or failed, the user's
answers to your questions, your reviews finished or failed, and responses to them. Attached
`hotbed run` is for short work only (a build, a quick test); anything long, `--detach` it and poll.
An attached `run` killed locally (a tool timeout, Ctrl-C) stops its command on the node too.

- `poll` returns at once with what happened since the session's last poll, or waits up to two
  minutes for something to happen. Nothing after two minutes means nothing happened yet: carry
  on, and poll again when you next want news. This machine keeps where its polls of each session
  left off (saved once a reply is in hand, so a reply lost to a restart loses nothing), so each
  event comes once, from the CLI or the MCP tool `poll` alike; `--after N` reads from cursor N
  instead (0: from the start).
- `hotbed logs -f CMD` still follows one command's output until it ends (redirect it to a file to
  keep its output out of your context); it exits with the command's status, or 124 when its soft
  time limit or someone paused it (see Time limits and Stopped or paused by hand).
- Calls ride out restarts, `poll` too.

Never write shell loops around `poll`, `ps` and `jq` that retry on any failure: when hotbed's
arguments or output change under them (`hotbed upgrade` replaces the CLI after every push), such
loops spin for hours on an error. Exit status 2 (`INVALID_ARGS`) means the call is wrong and will
never succeed; fix it.

## When the controller or a worker restarts

Deploys restart the controller and workers often. Every hotbed call waits up to three minutes for
one it cannot reach, saying so on stderr, so you need not retry. Every call, `run`, `sync`, `logs`
and `stop` included, goes through the controller (the Cloudflare Worker), which relays it to the
workspace's worker over that worker's outbound WebSocket tunnel; only a Mac's own worker is also
reachable over its local Unix socket. Workers listen on no network address, so with the controller
down nothing reaches them: calls wait for it. Only after the three minutes does a call fail with
`CONTROLLER_UNREACHABLE`: the controller is down, not restarting. Commands already running on
workers keep running meanwhile, and what happened reaches `poll` once the controller is back.

## run

`hotbed run [flags] -- CMD...` (session defaults from `HOTBED_SESSION` / `T3CODE_THREAD_ID` / git worktree; repo defaults from `HOTBED_REPO` / current git checkout; override with `-s` / `-r`)

- Attached (the default) is for **quick** work: it prints the command's output as it runs and exits
  with its status. Ctrl-C or killing `hotbed run` stops the command. It waits at most `--wait`
  (default **2m**; `0` for until it ends; with `--soft-limit` alone, until that pauses it, exiting 124): a command
  still running then is **left running, detached**, and `run` exits **125**, saying how to read
  it. Past `--max-output` (default **32K**, `0` for all) its output stops printing as it runs;
  its last lines print as it ends. Either way **all of it is kept**: read it with `hotbed output`.
  So **never pipe a run through `grep`, `tail` or `head`**: what you filter out is what you need
  when it fails. An attached run is admitted ahead of detached work and charged little memory
  (512 MiB, or what it holds): the fast lane.
- `--detach` is for **long** work and dev servers: it prints the command (with its ID) as an
  envelope and returns at once. Say what it needs: `--mem 8G` (the memory it holds at its peak,
  reserved for it) and `--expect 20m` (how long it should take); its end sets both beside what it
  took. Without `--mem`, the newest earlier run of the same command line in the project gives it;
  with neither, the run is refused. In a VM its output goes **to a terminal**: tools draw progress
  and color as for a person, and `hotbed output` settles what they redraw; its input stays
  `/dev/null`, so a prompt fails at once. `--tty` gives it the terminal for input too (it reads
  what `hotbed send CMD TEXT` types, and a prompt waits); `--no-tty` none. Its end arrives in
  `hotbed poll`.
- Dev servers: `--port 5173` puts its URL in the cockpit; `--ready 'listening'` (a regular
  expression its output will match) or `--ready-port` makes `run --detach` return once it is ready
  (at most `--ready-timeout`, default 2m), failing (NOT_READY) if it ends first. After an edit,
  `hotbed output CMD --since-sync` is what it logged since the edit reached it.
- `--kind build|test|check|server|shell`: classifies the run for history and cold-build metrics.
  Defaults to `server` when `--port` is set, otherwise `shell`; pass `--kind` explicitly when you
  want a run recorded as `build`, `test`, `check`, or `server`.
- `--priority low|normal|high|urgent|N`: sets scheduling priority (default `normal` / `0`; `low` =
  `-10`, `high` = `10`, `urgent` = `20`, or any integer in `-100..100`). Higher-priority queued
  commands admit ahead of lower-priority ones; nothing running is preempted for them.
- `--mem-low 2G` guarantees memory: the command queues until that fits. Without it, admission
  counts a command as needing 2 GiB, or the most it has held if more, so a node takes only as many
  as fit; pass a smaller `--mem-low` for light commands. `--mem-high 8G` throttles and swaps past
  that. Nothing is frozen or killed for memory: what is admitted runs, swapping if
  it must, and a full node holds new work until it has room. Stop a command by hand
  (`hotbed stop CMD`) if a node is truly overloaded.
- On the vm runtime each command gets a VM of its own on the workspace's disk, so a second command
  in the workspace waits for the first. `--vm-cpus 4` and
  `--vm-mem 16G` size its VM (default: the node's); they size the VM only, and the node reserves
  nothing for them. These flags send the run to a vm-runtime node.
- `hotbed run --fork` runs in a throwaway workspace of its own: a new one, forked from the
  project's base with your checkout synced in, deleted with whatever the command left in it once
  the command ends. It neither waits for your session's workspace nor changes it, so use it to run
  something beside a command already running there; what it builds is not kept for later runs.
- `--no-capture` opts a run out of auto-capturing its workspace as the project's shared warm base.
  `--capture-base` starts a capture-enabled workspace and uploads its disk as the project's shared
  warm base once the run exits 0. `--fresh` starts from the bare OS base image instead of the
  project's cached warm base.
- A saturated node holds new work: once its CPU pressure reaches 60%, commands queue until it is
  under 30% and has not reached 60% for 30 seconds (measurements, on CPUs of their own, are not
  held). The controller sends new workspaces to less loaded nodes. `hotbed ps` says why a command
  waits; `hotbed node show HOST` shows every signal against its limit.
- Time limits: see below. Otherwise commands run until they end or `hotbed stop CMD`.
- `--bench` runs it as a **measurement**, for numbers you will compare (benchmarks, timings). It
  waits for any other measurement on the node to end and for running work to fit beside it, then
  runs alone on the node's measurement CPUs (`hotbed nodes`: `benchCpus`), which no other command
  uses meanwhile, with IRQs and kernel work moved off them and, where the node can, its own cache
  and memory bandwidth, the other cores' clocks capped and other work's disk IO capped. It never
  swaps and is never parked. In a VM (the vm runtime), its VM boots on the measurement CPUs, a
  vCPU pinned to each (`--vm-cpus` asks fewer). If that isolation is lost mid-run it fails,
  saying so, whatever its status: rerun it. It goes only to nodes with `benchCpus`; compare only
  numbers from the same node, so narrow it to that node's architecture (`--arch amd64` for ubu1).
- Toolchains and caches (`RUSTUP_HOME`, `CARGO_HOME`, `GOMODCACHE`, `GOCACHE`, `XDG_*`) live
  inside the workspace, so the base's downloads and builds are already there.

## Reading output: output

`hotbed output CMD` (also the `output` MCP tool) reads a command's output, live or ended, as its
terminal shows it: progress bars and redraws collapsed to their last state, colors gone (`--raw`
for it as written). Every answer is bounded (`--max`, default 200 lines), numbers its lines, and
says what it left out and how to get it; it ends with the command's state and the line to read on
`--after`:

- `--grep PATTERN -C 3` (RE2, `-i` to ignore case): the matching lines with context; `--head N` /
  `--tail N` take the first or last N matches.
- `--tail N`, `--head N`, `--from N --to M`: lines by place (the default is its last lines).
- `--after N`: only what came after line N (each answer's `next --after N`).
- `--until PATTERN --timeout 600`: first wait for a line matching it (or the command's end), then
  answer, the matching lines marked: a test's result, a server's "listening".
- `--wait --timeout 600`: first wait for it to end.
- `--since-sync`: what it wrote since its workspace last synced (a dev server after your edit).

`hotbed detach CMD` lets an attached command run on with no one waiting (as `run` does after
`--wait`); `hotbed send CMD TEXT` types into one run with `--tty` (a newline follows unless
`--no-newline`). The cockpit's command page shows the output as lines and, for one in a
terminal, the terminal itself.

## Time limits

A command that hangs (a deadlocked test) must not hold you, or the node, for hours:

- **Soft limit** (`--soft-limit`, default **10m** for attached `run`): once the command has run
  that long without ending, it is **paused**: frozen, its memory pushed to swap. `run` returns,
  exiting 124, and says on stderr how to go on. The limit counts from its start and again from
  each time you start waiting on it: `hotbed logs -f CMD` resumes it and gives it another soft
  limit, exiting with its status if it ends meanwhile. Look at its output first
  (`hotbed logs -n 50 CMD`): if it is stuck, `hotbed stop CMD`.
- **Hard limit** (`--hard-limit`, default **2h** for attached `run`): that long after it started,
  paused or not, it is stopped (cancelled).
- `0` turns either off, but a soft limit always comes with a hard one (default 2h): a paused command
  no one comes back for must end. `--detach`ed and scheduled commands have no limits unless given;
  a measurement (`--bench`) is never paused, so takes only `--hard-limit`.
- A paused command gives back its guaranteed memory (`--mem-low`); resumed, it runs at once,
  swapping back in what it must.
- **Little CPU for much memory**: a running command that reserves over a quarter of the node's
  memory (its `--mem`, its estimate, or the most it has held, whichever is most: what makes other
  work queue) while keeping less than a core busy for each 6 GiB of it, averaged over 5 minutes, is
  stopped (cancelled), whatever its time limits, its diagnostics taken first (`hotbed diagnostics
  CMD`); its reason says so. Dev servers (`--port`), interactive (`--tty`) commands and
  measurements are exempt; `--keep-idle` keeps one that rightly holds much memory for little CPU.
- An attached run waits only `--wait` (2m) before leaving its command running, detached, which
  drops its soft limit: a soft limit pauses a command only while you wait on it. A long build or
  test suite you expect to take 25 minutes: `--detach --mem 8G --expect 25m`, then
  `hotbed output CMD --wait --timeout 600` or `hotbed poll`; or wait on it attached with
  `--wait 30m`.

## Stopped or paused by hand

The user (from the command's cockpit page) or an agent (from the CLI) can stop or pause a command
with a comment and optional diagnostics (an activity sample and a full Linux inspection). Your
attached `run` or `logs -f` then ends with who did it, their comment, and how to read the
diagnostics:

- **Stopped**: fails with `COMMAND_STOPPED`; if diagnostics were taken, the message names
  `hotbed diagnostics CMD`.
- **Paused**: exits 124 saying `CMD was paused by …`. Unlike a soft-limit pause, waiting on it
  (`hotbed logs -f`) does **not** resume it: it stays paused until resumed (`hotbed resume CMD`, or
  the cockpit). Read the comment and the diagnostics, and answer the user (`hotbed question add`)
  before resuming it yourself.

`hotbed diagnostics CMD [--seq N | --after-seq N]` (also available as the `diagnostics` MCP tool)
prints the saved diagnostics from the controller (oldest first: intervention metadata, activity
sample text report, and inspection summary; `--json` for the full structured payload including
`hasMore` and `nextAfterSeq`, and `--seq N` / `--after-seq N` to fetch a specific sequence number
or paginate through large diagnostic sets), live or long after the command ended. You can do the
same from the CLI: `hotbed stop|pause -m COMMENT --diagnose CMD`, and `hotbed resume [-m COMMENT]
CMD` (comments are capped at 2048 bytes).

## poll

`hotbed poll` (`--json`) returns the session's events, each with its kind's own fields:

```json
{"version":"1","ok":true,"command":"poll","data":{
  "events":[
    {"seq":"12","at":"1795300000000","sessionId":"brave-otter","detail":"",
     "command":{"what":"WHAT_EXITED","commandId":"k3m9x2qa","argv":"cargo nextest run",
                "workspaceId":"q7n2c4ze","state":"COMMAND_STATE_FAILED","exitCode":100}},
    {"seq":"13","at":"1795300004000","sessionId":"brave-otter","detail":"question 4 (Store) answered: SQLite",
     "question":{"what":"WHAT_ANSWERED","question":{"id":4,"state":"QUESTION_STATE_ANSWERED","answer":{"picked":["SQLite"]}}}},
    {"seq":"14","at":"1795300009000","sessionId":"brave-otter","detail":"review 7 (Retry on lost leases) is done: One race.",
     "review":{"what":"WHAT_FINISHED","reviewId":7}}],
  "cursor":"14","live":1,"ended":false}}
```

Each event is one of:

- `command`: `what` is `ADMITTED`, `PARKED`, `THAWED`, `PAUSED`, `RESUMED`, `ISOLATION_LOST`,
  `EXITED` (with `exitCode`) or `CANCELLED`.
- `workspace`: `READY` or `FAILED`.
- `question`: `ANSWERED` or `DISMISSED`, with the question as it is now; act on it, then
  `hotbed question ack ID`.
- `review`: `FINISHED`, `FAILED` or `RESPONDED`; `hotbed review show ID` has the rest.

`detail` says what happened in a line. `live` counts commands not yet ended. An empty `events`
after two minutes means nothing happened yet. `ended: true` means the session is over
(`endReason` says why).

## Output

Commands print concise plain text by default (formatted via Go templates declared on each RPC).
Pass `--json` for one JSON envelope on stdout, or `--jq EXPR` to filter the response JSON directly:

```json
{"version":"1","ok":false,"command":"run","error":{"code":"REPO_UNREACHABLE","message":"...","suggestion":"check the repo's URL (hotbed repo ls) and that this node can reach it; retry once it can","retryable":true}}
```

Act on `error.code`, never the message. `retryable: true` means the same command can succeed later
unchanged; otherwise `suggestion` says what to change. Exit status: 0 ok, 1 error, 2 bad arguments,
130 interrupted (Ctrl-C). `run` and `logs -f` exit with the command's status (124 when its soft
time limit or someone paused it), and hotbed's own failures go to stderr.

Every session, workspace, command and node in the JSON output carries `cockpitUrl`, its page in the
cockpit (the controller's UI): give it to the user when they want to watch or look into it. A
command's `url` is something else, the dev server it serves.

Generated proto CLI reference: [references/commands.md](references/commands.md) (`hotbed skills get hotbed/references/commands.md`).
Codes and what to do: [references/errors.md](references/errors.md) (`hotbed skills get hotbed/references/errors.md`).
Setting up a cluster: [references/setup.md](references/setup.md) (`hotbed skills get hotbed/references/setup.md`).
Every other command (sessions, projects, nodes, diagnostics, auth, config): [references/other-commands.md](references/other-commands.md) (`hotbed skills get hotbed/references/other-commands.md`).
