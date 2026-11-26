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

## The loop

```sh
hotbed session start                             # prints active memories & binds to T3CODE_THREAD_ID / worktree
hotbed run -- go vet ./...                       # quick work: placed on a node, syncs your edits there, runs, prints output, exits with its status
                                                 # (125 if still running after 2 min: left running, detached; see run)
hotbed run --detach --mem 8G -- cargo nextest run  # long work: say what it needs; prints the command's ID and returns
hotbed poll                                      # what happened since the last poll: commands ended, answers, reviews (up to 2 min)
hotbed output CMD --grep 'FAIL|panicked' -C 3    # read its output, kept whole: never filter a run's output as it runs
hotbed session end                               # ends session and reminds you to save any lessons learned
```

Do not pipe `hotbed session start` into `jq` or swallow its stdout: it prints active memories directly and auto-binds the session so `-s` is not needed.

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
  any machine. In a thread or
  worktree already bound to a live session it fails with `CONFLICT` naming that session: keep
  using it, end it first, or pass `--replace` to end it and bind the new one.
  `session start NAME --resume` takes up session NAME again: live again if it ended (with no
  workspaces), as it is if not. It keeps its project (and associates `T3CODE_THREAD_ID` if none was
  set yet).
- **The controller places every run.** Each `hotbed run` goes to the node it picks by load: it
  keeps a session where it already has a workspace of the repo unless that node is clearly busier
  than another, and otherwise prefers a node with a warm base. Leave the choice to it, so work
  spreads over the cluster. Narrow it only when the run needs something: `--arch amd64|arm64` for
  an architecture, `--node HOST` for one machine (such as the one earlier measurements came
  from). `--bench` and `--mem-low` narrow it by themselves. stderr says where it ran.
- **A workspace per node, of the whole project, made as needed.** A workspace holds every repo
  of the session's project, each checked out at `/work/REPO`, side by side. The first run of a
  session on a node forks the project's base there, with each repo at its branch's head. Structural
  changes (first base on a node, setup step edits, repo list changes, or `hotbed project rebuild`)
  freshen immediately. Commit-only branch movement on an existing warm base is an optimization
  capped by duty cycle (default 5% of wall time, 30m minimum interval per project, 6h cooldown cap,
  15m exponential failure backoff) and deferred to an idle, low-priority (`-10`) background slot
  for syncing checkouts so your workspace forks immediately without waiting. Tune org-scoped caps
  and settings live from the Cockpit (`/settings`) or via `hotbed config set` (`freshen.*`). A run
  starts in `/work/REPO` of the repo it is for, or in `/work` when it is for none (outside any
  checkout, no `-r`). Each workspace keeps its state between the commands run in it. On the vm
  runtime each command runs in a fresh VM booted on the workspace's disk and stopped when it ends,
  so a second command of the session on that node waits until the first ends.
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
  node is offline it fails retryably rather than start over; `--node`/`--arch` naming another
  node start a fresh workspace there.

## Memories & learning

Memories are cross-agent rules and playbooks shared across every agent harness and machine. New
memories are automatically approved and active immediately; users can edit any field, promote a
project memory to global, archive/restore, reorder, or permanently remove them in the cockpit or CLI.

```sh
hotbed session start                             # prints active always-on rules + on-demand index
hotbed memory get zsh-and-macos-shell-footguns   # fetch full markdown body of one or more memories
hotbed memory ls [-q QUERY] [--scope global|project] [--all-projects]
hotbed memory add --global \
  --slug short-replies \
  --summary "Keep chat replies to a few lines; put long detail in files or artifacts." \
  --rationale "User correction: they don't read long replies"
hotbed memory add --slug zfs-base-freshen \
  --summary "ZFS clone/snapshot base freshening internals — read before debugging base failures." \
  --rationale "Hit ZFS clone flake while debugging base freshening" \
  --body-file - <<'EOF'
...detailed markdown playbook...
EOF
hotbed memory add --supersede 4 --slug short-replies --summary "..." --rationale "..."
hotbed memory edit 4 --summary "..." [--global | -p PROJECT] [--repo REPO] [--rationale "..."]
hotbed memory promote 2                          # promote a project-specific memory to global
hotbed memory archive 2 --reason "..."           # soft-retire (restorable via `memory restore`; --reason required for agents)
hotbed memory rm 2                               # permanently remove a memory
```

- **Save lessons when you resolve them (not just in local files):** whenever you resolve a
  non-obvious bug or repo/tooling quirk, the user corrects you, or the user says "remember this" /
  "add to memory" / `/learn`, save it with `hotbed memory add` (alias `memory propose`, MCP:
  `memory_add`) so every future session gets it on `hotbed session start`. Agents must pass
  `--rationale "..."` explaining why the memory is being added and what triggered it; this
  rationale is stored as metadata only and is not returned in context when reading memories.
  `hotbed session end` prompts you at wrap-up.
- **A memory is the last resort.** Every always-on memory costs every session's context, so first
  put the lesson where it will be found: fix the bug or tooling quirk itself (a script, a check, a
  clearer error); write a design decision as a comment beside the code it constrains, or in the
  repo's `AGENTS.md` (a subdirectory's own `AGENTS.md` for rules about that part); and put how to
  use a tool in that tool's skill. Save a memory for what has no such home: the user's
  preferences, facts about machines, and workarounds for open bugs (name the bug's task so the
  memory can go once it is fixed). Check `hotbed memory ls -q WORD` first and update a memory that
  already covers it; keep each memory to one rule, so one can be retired without the others.
- **Archive outdated memories with a reason:** when a memory is obsolete or no longer true,
  agents can soft-retire it with `hotbed memory archive ID --reason "..."` (MCP: `memory_archive`;
  `--reason` is required for agents). Archived memories can be restored with `hotbed memory restore`.
- **Automatic delivery mode:** omit `--mode` and hotbed picks automatically:
  - No `--body` → `always`: short guardrail/preference (≤ 240 chars) printed inline on every
    `hotbed session start` (and in `hotbed mcp` server instructions).
  - With `--body` / `--body-file` → `on_demand`: 1-line summary + trigger on `session start`; fetch
    details with `hotbed memory get SLUG [SLUG...]` (or `global/SLUG`, `PROJECT/SLUG`, `#ID`).
- **Two scopes:** defaults to your session's project (and repo if held by the project); pass
  `--global` for cross-project preferences and workflows, or `-p PROJECT` for a lesson that
  belongs to another project than your session's. Use `hotbed memory promote ID` (or
  `hotbed memory edit ID --global`) to promote a project memory to global. A project's memory
  can supersede only global memories and its own project's; a global one can supersede any.
- **Update & condense, don't duplicate:** if a slug already exists, hotbed tells you its `#ID` so
  you can pass `--supersede ID` or edit in place with `hotbed memory edit ID` (supporting
  `--clear-title`, `--clear-body`, `--replace-tags`, `--replace-content`, `--repo`, `--rationale`).
  `hotbed memory show REF` prints provenance, supersession links, and full revision history.

## Missing a tool or dependency? Add a step

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

## Asking the user: questions

When you need the user's decision, don't stop and wait: ask on the project's question list, go
ahead with your best guess, and read the answer when it comes. A question takes the JSON
AskUserQuestion takes, plus a little; each of its questions becomes an entry of its own:

```sh
hotbed question add <<'EOF'
{"questions": [{"question": "Which store should sessions use?", "header": "Store", "multiSelect": false,
  "body": "Sessions outgrew `sessions.json`: **4 MB**, rewritten on every change.",
  "options": [{"label": "SQLite", "description": "One file, already used", "implemented": true},
              {"label": "Postgres", "description": "A server to run"}]}]}
EOF
hotbed poll                      # the answer arrives as an event, the question with it; keep working meanwhile
hotbed question outbox           # answers waiting for an agent
hotbed question ack 12           # you read it and acted on it: it is handled
```

- **Send the user its link.** Each question `question add` returns carries its cockpit page
  (`cockpitUrl`, printed under it), where the user answers it: when you want an answer, put the
  link and the question in plain words in your reply, not just its number.
- **Poll before you assume no answer.** The user answers whenever they like; an answer reaches
  you only through `hotbed poll`. Before you end your turn, commit, or conclude the user has not
  answered, poll. Every question command's response carries a `reminder` saying so.
- **Say what you went ahead with.** Mark the option you built `"implemented": true`: going ahead
  and marking it is the norm, so the user sees what changes if they answer otherwise. Mark
  `"recommended": true` explicitly for a way forward you have not built: option labels are
  delivered verbatim (no `"(Recommended)"` suffix stripping). One of each at most unless
  `multiSelect`; never both on one option.
- **Record a decision you took without asking** with `question add --answered`: the same JSON,
  what you decided marked `"implemented": true` (required) and what else was possible as the
  other options, why and how to undo it in `body`. It is answered with its implemented options and
  handled at once, by your session: never in the outbox, waking no one. The user reopens and
  answers it to overturn it, which reaches the outbox as any answer does.
- **Keep the title short.** `question` is one sentence; the setting, findings and stakes go in
  `body`. Title, body, labels and descriptions are markdown; `preview` is shown as it is.
- **One list per project, every session's.** `question ls` lists your project's questions, newest
  first (`--sort oldest|updated|rank`, `--state open|answered|dismissed|handled|deleted|all`,
  `--asked-by SESSION`, `--t3code-thread-id ID`, `-p PROJECT`, `--all-projects`). Read it before
  asking: another session may have asked already.
- **Sessions and T3 Code threads are mapped on every question.** Each question carries its
  `t3codeThreadId` (captured from `T3CODE_THREAD_ID` when asked or resolved from its asking
  session), and `question outbox` / `question ls` also return a `sessions` summary (session ID,
  project, liveness, `t3codeThreadId`, matched `questionIds`, and state counts) so you can peek at
  the outbox (`hotbed question outbox --all-projects`) and immediately see which sessions and T3
  Code threads have answers waiting.
- **The user answers in the cockpit**, or dismisses (no answer will come). An answer waits in the
  outbox until an agent acknowledges it; any agent of the project may act on and ack an answer.
  An answer or dismissal is also an event of the asking session: its `hotbed poll` returns it,
  with the question as answered.
- **Any session may edit (`question edit ID`, while open) or delete (`question rm ID --reason`)
  any question** it finds wrong or no longer needed. Nothing is lost: `question show ID` has the
  whole history, each change with its session and time.
- A question records where you asked from (checkout, branch, commit, changed files) and, if the
  project holds your checkout's repo, that it is about that repo (`--repo REMOTE` names another).
- A session changes only its own project's questions (edit, rm, ack, answer, dismiss, revise,
  reopen, move).
- The user may change a response still in the outbox, return any answered or handled question to
  the inbox (`question reopen ID`, which clears its response) and revise any question
  (`question revise ID`, JSON as `question edit` takes): read the outbox again before acting.
- Never answer a question yourself unless the user told you the answer: then pass it on with
  `question answer ID --pick LABEL [--other TEXT] [--notes TEXT] -s SESSION`, or `question dismiss
  ID --reason WHY`; `question reopen` and `question revise` likewise only at the user's word.
  `question move ID --first|--last|--before ID` reorders the user's list within the question's
  project. `question projects` counts every project's open and outbox questions.

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

## Reviews: asking reviewer agents, and the user

A review is of an exact version: the commits it names never move. Both the implementer's exact model
and every reviewer's exact model are recorded; multiple reviewers (in separate sessions or as
sub-agents sharing one session) can review the same review. Ask for one in the checkout to review,
then give its number to one or more reviewer agents:

```sh
hotbed review request --model gemini-4-neon --title "Retry on lost leases" --instructions "look for races"
                                 # --model names the implementer's exact model (or inherited from `session start --model`)
                                 # base: where HEAD left origin's default branch (--base REF, --whole)
                                 # head: HEAD, or a snapshot commit of uncommitted changes, pinned
hotbed poll                      # each reviewer's end arrives as an event; keep working meanwhile
hotbed review show 7             # implementer, reviewers, findings (with locations, decisions, quick_by and responses)
hotbed review accept 7 1 3 --reason "fixing"
hotbed review dismiss 7 2 --reason "pre-existing; not this change"   # alias: reject
hotbed review wont-fix 7 4 --reason "intentional tradeoff; documented"
hotbed review fixed 7 1 --commit HEAD --where lease.go:80-95 --notes "renew holds the lock"
                                 # handled: the commit in full, the files and notes optional;
                                 # the user can respond to it no more (reopen if the fix fails)
hotbed review invalid 7 3 --evidence "expiry checks the clock under the lock (lease.go:41)"
                                 # not a real defect, with Markdown evidence; the user may dispute it
hotbed review link 7             # the review's page in the cockpit: send it to the user
hotbed review responses 7        # the responses since you last read them (poll says when one comes)
```

Each reviewer (in its own session, or as a sub-agent sharing the parent session):

```sh
hotbed review start 7 --model claude-opus-4-6 [--subagent verifying-reviewer]
                                 # claim/join the review, naming your exact model ID (required);
                                 # multiple models or sub-agents may start on the same review
git diff BASE HEAD               # the commits are in the requester's checkout (refs/hotbed/reviews/HEAD)
hotbed review findings 7 [--reviewer 1] <<'EOF'
{"findings": [{"short_summary": "Lease renewal races expiry", "summary": "...", "failure_scenario": "...",
  "category": "correctness", "severity": "important", "verdict": "CONFIRMED", "file": "lease.go", "line": 88,
  "locations": [{"path": "renew.go", "start_line": 12, "end_line": 20, "excerpt": "..."}]}]}
EOF
hotbed review finish 7 [--reviewer 1] --summary "One race, one nit."   # or: hotbed review fail 7 --reason WHY
```

- When multiple reviewers are active in the same session (e.g. concurrent sub-agents sharing
  `T3CODE_THREAD_ID` / `HOTBED_SESSION`), pass `--reviewer ID` (or `--model` / `--subagent`) to
  `findings`, `finish` and `fail` so each finding and report is attributed to the right reviewer.
- A finding is ReportFindings' JSON (severity `important`, `pre_existing` or `nit`; verdict
  `CONFIRMED`, `PLAUSIBLE` or none), with any number of `locations` besides, or none. Put the
  lines in `excerpt`: the cockpit shows them without the code. Found nothing? Report
  `{"findings": []}`: the review's events say so.
- A reviewer that cannot reach hotbed (in a network-isolated sandbox, such as `codex exec
  --sandbox read-only`) ends its report with its findings in a fenced block whose info string is
  `json hotbed-findings`, holding that same `{"findings": [...]}` (`[]` when none survive). Its
  caller records it with `--model` (and `--subagent` if sharing a session), and the review keeps
  the whole report for the cockpit:

  ```sh
  hotbed review start 7 --model gpt-5.5-codex --subagent codex-exec
  codex exec --sandbox read-only --output-last-message report.md - < instructions.md
  hotbed review findings 7 --model gpt-5.5-codex --from-report report.md
  hotbed review finish 7 --model gpt-5.5-codex --summary "$(head -1 report.md)"
  ```
- Every decision and response explicitly records `actor_kind` (`USER` vs `AGENT`) plus the agent's
  exact `model`, `session`, optional `subagent`, and `t3code_thread_id`. `hotbed review respond`
  uses your bound agent session by default; pass `--as-user` only when recording the human user's
  explicit word. On findings, `quick_by` shows whether the user clicked `Verify` / `Fix if verified`
  / `Won't fix` or an agent set it.
- `hotbed review ls` lists the project's reviews (the session's, or the one holding this checkout's
  repo); `review cancel` withdraws one; `review reopen`
  makes a decided finding open again. Nothing is removed: `review show` has every event.

## Keeping a session alive

A session nothing touches for **an hour** is abandoned: its commands stop and its workspaces are
deleted. Every hotbed call that names the session counts, and so does an attached `hotbed run` or
`hotbed logs -f` for as long as it waits on its command.

So never write a keep-alive loop. `hotbed poll` counts too: an agent polling for its work keeps its
session. A detached command nothing polls or waits on is what the hour is for: an agent that went
away leaves no dev server behind.

## hotbed as MCP tools

`hotbed mcp` serves hotbed's commands as MCP tools over stdin and stdout (`question add` is the tool
`question_add`, its flags and arguments the tool's parameters), for an agent to start as a local
MCP server, e.g. `claude mcp add hotbed -- hotbed mcp`. The server is a session: it takes the one
`HOTBED_SESSION` or this T3 thread or git worktree names (resumed if it ended), or starts one bound
here, so `hotbed` commands run here use it too. It keeps the session alive while it runs, ends it
when it exits, and the next server started here resumes it. Its tools never take a session.

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
one it cannot reach, saying so on stderr, so you need not retry. `run`, `sync`, `logs` and `stop`
talk to the workspace's machine directly: with the controller down they keep working without
waiting for it (`run` goes to the workspace this machine last ran in), and the session stays alive
while you do. Only after the three minutes does a call fail with `CONTROLLER_UNREACHABLE`: the controller is
down, not restarting. What happened meanwhile reaches `poll` once it is back.

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
  swaps and is never parked. If that isolation is lost mid-run it fails, saying so, whatever its
  status: rerun it. It goes only to nodes with `benchCpus`; compare only numbers from the same
  node, so pin it with `--node`.
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

## Scheduled commands

Recurring work, such as a nightly measurement, runs by itself:

```sh
hotbed schedule add nightly -r app --cron '0 2 * * *' --node worker1 \
  --env BENCH_ENV=nightly -- scripts/nightly.sh
hotbed schedule show nightly                # the command, the next run, its runs and their sessions
hotbed schedule run nightly                 # a run now; prints its session, for hotbed poll -s and logs
```

- **Each run is a session of its own**, `NAME-YYYYMMDD-HHMM`, with a fresh workspace of the repo
  at its branch's head, in which the command runs detached at `/work/REPO`. It takes `run`'s
  flags (`--env`, `--mem-low`, `--mem-high`, `--kind`, `--node`, `--arch`). Nothing persists between runs except what
  the command keeps outside the workspace (pushed to git, or on the node: pin it with `--node`).
- **Its session's end.** A run is never abandoned while its command runs. One that succeeds ends
  its session. One that fails keeps it for the idle hour, so you can `hotbed run -s SESSION -r REPO --
  ...` in its workspace to look; its log stays (`hotbed logs COMMAND`).
- **Cron** has five fields in the controller's time zone (`CRON_TZ=Europe/Lisbon 0 2 * * *` for
  another). Runs never overlap: a slot that comes while the previous run is still going is
  `skipped`. A slot that passes while the controller is down is `missed`, not run late.
- `schedule add` with an existing name replaces it. `schedule ls [-p PROJECT | --all-projects]`
  lists the active project's schedules by default. `schedule pause`, `resume`, `rm`.

## Other commands

| Command | Does |
|---|---|
| `hotbed session status [--t3code-thread-id ID]` | Active session state, idle time, workspaces, and commands (`--t3code-thread-id`: look up the live or latest session for a T3 Code thread on the controller) |
| `hotbed session current` | Active session and where it came from (`flag`, `env`, `thread`, `worktree`) |
| `hotbed session ls [--ended] [--all-projects] [--t3code-thread-id ID]` | The project's live sessions (`--ended`, `--all-projects`, `--t3code-thread-id`) |
| `hotbed session search [THREAD_ID] [--t3code-thread-id ID] [--live] [-p PROJECT] [-n N]` | Search persistently across every project (and ended sessions of any age) for sessions associated with a T3 Code thread ID (default: `T3CODE_THREAD_ID`), live ones first then most recently active |
| `hotbed shell [--node HOST] [--arch ARCH] [--sync] [--env K=V] [-- CMD...]` | Open an interactive shell (over SSH) in the session's workspace (reuses an existing workspace when it has one; `--sync` pushes local checkout edits first) |
| `hotbed project ls` | Projects, their repos, live sessions, schedules, reviews and questions |
| `hotbed project show NAME` | A project and every change to it |
| `hotbed project bases [PROJECT] [--node HOST] [--state STATE]` | Every node's bases for a project (or every project), newest generation first per node, with commits, freshen command ID, and failed step |
| `hotbed freshen ls [PROJECT] [--node HOST] [--state STATE] [--active] [--runtime RT] [--cold] [--since 24h] [-n N]` | Active and historical (30-day) base freshens across all runtimes (`vm`, `cladefs`), with cluster-wide timing breakdowns (prep, queue, git, steps, parked, finalize/R2-sync), slowest steps ranking, per-node/project cold & failure streaks, and waiting workspaces |
| `hotbed freshen show ID [--node HOST]` | Deep autopsy of one base freshen (by freshen command ID or base ID): cold reason, phase & step timings, memory guarantee vs peak, waiting workspaces, command timeline, live node admission snapshot, and related events |
| `hotbed project rm-repo NAME --repo REMOTE` | Take a repo out of a project |
| `hotbed schedule add ... -p PROJECT` | A schedule's runs work on its project (default: the one holding its repo) |
| `hotbed schedule ls [-p PROJECT \| --all-projects]` | The project's schedules (`--all-projects`: every project's) |
| `hotbed ps [-p PROJECT \| --all] [-e] [-m TEXT] [-n N]` | The session's commands, live and just ended (`-p`: a project's, `--all`: every session's; `-e`: ended ones too, from 30 days of history; `-m`: only those whose command line holds TEXT; `-n`: the newest N ended ones, default 50, 0 for all; running ones always show) |
| `hotbed output CMD [--grep P -C N] [--head N \| --tail N] [--from N --to M] [--after N] [--until P \| --wait] [--timeout S] [--since-sync] [--raw] [--max N]` | Read a command's output, kept whole, as its terminal shows it: bounded answers that say what they left out; `--until` / `--wait` wait first (MCP tool `output`) |
| `hotbed detach CMD` | Let an attached command run on, detached (MCP tool `detach`) |
| `hotbed send CMD TEXT [--no-newline]` | Type into a command run with `--tty` (MCP tool `send`) |
| `hotbed issue report TITLE [-m DETAILS]` / `issue ls [-q WORDS] [--archived \| --all]` / `issue show ID` / `issue upvote ID [-m COMMENT]` / `issue archive ID [-m WHY]` / `issue reopen ID` | Problems with hotbed itself: report one once, +1 it after (MCP tools `issue_report`, `issue_ls`, …) |
| `hotbed logs CMD [-f \| -n N]` | A command's output, any session's by its ID; `-f` (the session's own command: it waits on it) follows until it ends, `-n N` its last N lines (at most 1 MiB of them; stderr says when it cut). A log keeps its first 16 MiB and last 240 MiB, with a `[hotbed: … dropped here …]` line between; logs of ended commands are kept 7 days |
| `hotbed stop CMD [-m COMMENT] [--diagnose]` | Stops a command of the session (another session's: `-s SESSION`); with `--diagnose`, takes its diagnostics first |
| `hotbed pause CMD [-m COMMENT] [--diagnose]` | Pauses a command by hand: frozen until `hotbed resume`, waiting on it or not; with `--diagnose`, takes its diagnostics first |
| `hotbed resume CMD [-m COMMENT]` | Resumes a paused command |
| `hotbed diagnostics CMD [--seq N \| --after-seq N]` | The diagnostics taken of a command as someone stopped or paused it (activity sample and inspection), live or ended; supports `--seq` / `--after-seq` pagination and the `diagnostics` MCP tool |
| `hotbed inspect [procs\|files\|cgroup\|kstack\|maps\|limits] CMD` | Opt-in Linux `/proc` & cgroup v2 inspection of a live command |
| `hotbed sample CMD [-d SEC] [--pid PID] [--text]` | Activity Monitor-style Linux activity sample of a live command (CPU/IO deltas, user & kernel stacks, syscalls, open files/sockets/locks, cgroup/PSI deltas & perf counters); each is kept with the command, and stderr names its number |
| `hotbed samples ls CMD` | The activity samples kept of a command (its newest 32, each trimmed to 1 MiB), live or ended: number, when, window, PID, who took it |
| `hotbed samples show CMD N` | One kept activity sample in full, as `hotbed sample --text` prints it (`--json` for the sample itself) |
| `hotbed trace [strace\|perf\|stat\|stacks\|bpf] CMD` | Opt-in active Linux tracing (`strace`, `perf`, thread stacks, `bpftrace`) of a live command |
| `hotbed profile CMD \| --node HOST [--last 5m] [--query Q] [--format json\|folded\|top]` | Query the always-on OpenTelemetry eBPF continuous profile of a command (live or finished) or host-wide across a node. Its JSON lists each frame once, in `frames`; stacks, functions and buckets name frames by index there (`folded` and `top` print them by name) |
| `hotbed workspace ls [-p PROJECT \| --all]` | The session's workspaces (`-p`: a project's, `--all`: every session's) |
| `hotbed workspace show` | The session's workspace, as its worker has it now (`--node` when it has several) |
| `hotbed workspace files` | Every repo's files, below `/work` (`REPO/path`), each `added`/`modified` against the base or unchanged |
| `hotbed workspace file REPO/PATH` | A file's text and the lines it changed against the base |
| `hotbed workspace release` | Gives the workspace up; the session goes on, and its next run gets a new one |
| `hotbed repo add ID URL [-p PROJECT]` | Registers a repo, attaching it to `PROJECT` (or auto-creating a project for it) |
| `hotbed repo ls [-p PROJECT]` | Repos: where each project's code comes from (`-p`: only a project's repos) |
| `hotbed auth login [-w] [--name NAME] [--repo OWNER/REPO]` | Log in to GitHub on the controller via OAuth Device Flow (like `gh auth login`); delegates git HTTPS, git SSH, and `gh` to every worker, session, and command |
| `hotbed auth login-start [--name NAME] [--repo OWNER/REPO]` | Start a GitHub OAuth Device Flow and print the one-time code and URL for `auth login --login-id` |
| `hotbed auth set NAME [--token-file -] [--repo OWNER/REPO]` | Store a centralized git access token on the controller (headless/CI alternative to `auth login`) |
| `hotbed auth ls [--host HOST]` | List centralized git credentials configured on the controller (metadata only; secrets are never returned) |
| `hotbed auth rm NAME` | Remove a centralized git credential from the controller |
| `hotbed auth token REPO_URL` | Mint a short-lived git access token from the controller (auto-refreshing expiring OAuth tokens) |
| `hotbed auth api PATH [-X METHOD] [--body JSON]` | Proxy a GitHub REST or GraphQL API call through the controller's credentials |
| `hotbed step add\|set\|ls\|show\|rm [-p PROJECT]` | The project's setup steps (default: the session's project), and which each node's newest base ran |
| `hotbed project rebuild PROJECT` | Every node builds the project's next base from nothing |
| `hotbed project hosts PROJECT [HOST...]` | The nodes that keep the project's bases (none: every worker) |
| `hotbed nodes` | The cluster's machines, and the build each runs; a worker that cannot open its storage (a ZFS pool not imported, its disk missing) is offline with the reason (`storageError`), waits for it, and starts once it opens |
| `hotbed node show HOST` | Whether the node starts new work or holds it, and why: each signal (CPU pressure, memory pressure, memory reserved, disk free) against its limit, the work it held or made wait, and the placements that weighed it |
| `hotbed node retire HOST` | Retire a node gone for good (its machine or VM deleted): it leaves `hotbed nodes`, the cockpit and placement; its history stays. Refused while it is online |
| `hotbed config show [HOST]` | Show controller-managed cluster default and per-node worker/VM configuration (plus merged effective config when `HOST` is given) |
| `hotbed config set [HOST] [--replace] [--if-revision N]` | Merge (or replace) controller-managed cluster defaults or per-node worker/VM configuration (VM tuning, managed guest kernels, `hotbed-guestd`, FUSE settings) from JSON on stdin or `--file` |
| `hotbed config rm HOST \| --defaults` | Remove controller-managed per-node overrides for `HOST`, or clear cluster-wide defaults with `--defaults` |
| `hotbed placements [--node HOST] [--session S]` | Where the controller put each workspace and why: every node's cost in parts, or why it was ruled out (kept a day) |
| `hotbed events [--node HOST] [--kind KIND ...]` | The cluster's events, newest first; `--kind hold --kind wait --kind unhold` is every time a node held work and why |
| `hotbed daemon-logs [--source controller\|HOST] [--level warn\|error] [--since 1h] [-q Q] [-n N]` | Rotating WARN/ERROR slog logs from the controller and every worker, newest first (with rotation disk stats) |
| `hotbed snapshot` | Everything the cockpit shows, as JSON |
| `hotbed upgrade` | Install the controller's build over this hotbed (a CI build does it by itself) |
| `hotbed system upgrade [HOST] [--force] [--dry-run]` | Explicitly upgrade host `cladefsd`, `firecracker`, and managed guest kernel binaries from controller config (`system.cladefsd`, `system.firecracker`, `vm.kernels`); drains and terminates live commands, shells, and workspaces on each node before restarting `cladefsd` |
| `hotbed login [CONTROLLER] [--device] [--org ORG]` | Sign in to a WorkOS-enabled controller with Google (browser login, or `--device` for SSH/headless) |
| `hotbed logout [CONTROLLER]` | Remove locally stored WorkOS credentials for a controller |
| `hotbed whoami` | Show the signed-in user, active organization, role, and org memberships |
| `hotbed org ls\|switch\|create\|members\|invite` | List, switch, or create WorkOS organizations (one isolated Controller DO per org), and view or invite org members |
| `hotbed org join-token create\|ls\|rm` | Mint (`hbj_<org>.*`), list, or revoke org-scoped worker join tokens |
| `hotbed org api-token create\|ls\|rm` | Mint (`hbu_<org>.*`), list, or revoke org-scoped personal API tokens for `HOTBED_TOKEN` |
| `hotbed api ENDPOINT [-X METHOD] [-f k=v] [-F k=v] [-d DATA] [--input FILE] [-H K:V] [-i] [--jq EXPR]` | Make an authenticated HTTP or ConnectRPC request to the controller (e.g. `hotbed api /healthz`, `hotbed api ClusterService/GetSnapshot --jq '.nodes \| length'`) |
| `hotbed daemon [run\|install\|uninstall\|status]` | Unified local Hotbed host service: keeps an outbound `/ws/questions` stream open to wake idle local T3 Code threads when questions are answered or dismissed, runs a Mac's worker and machined (which runs its workspaces' VMs), and shows a native macOS menubar status app (`install` sets up one LaunchAgent on macOS or systemd user service on Linux, migrating away from legacy separate services) |

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
