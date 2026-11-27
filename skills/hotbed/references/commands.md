<!-- Code generated from proto/hotbed/v1/hotbed.proto via `go generate ./skills`. DO NOT EDIT. -->

# Proto-Generated CLI Commands

Every command below is generated directly from `proto/hotbed/v1/hotbed.proto` and also exposed as an MCP tool (`hotbed mcp`).
Pass `--json` for the JSON envelope, or `--jq EXPR` to filter the response JSON.

## `nodes`

### `hotbed nodes`

List the cluster's workers.

*MCP tool: `nodes`*

## `repo`

Repos the cluster keeps built bases of (add, ls, rm).

### `hotbed repo ls`

List repos registered with the cluster.

*MCP tool: `repo_ls`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `-p`, `--project` |  | Only repos held by this project; empty for every repo. |

### `hotbed repo add ID URL`

Add or update a repo; its project's workspaces check it out at /work/<name>. Give the project setup steps with hotbed step set.

*MCP tool: `repo_add`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `-p`, `--project` |  | Place the repo in this project (making it if needed; default: auto-create a single-repo project unless one holds its remote already). |
| `ID` |  | Short name, such as ratel; workspaces see the checkout at /work/<name>. |
| `URL` |  | Git URL to clone, in any form (https://github.com/o/r.git, git@github.com:o/r.git). |
| `--branch` |  | Branch bases follow (default: the remote's default branch). |

### `hotbed repo rm ID`

Remove a repo; workers delete its bases once no workspace uses them.

*MCP tool: `repo_rm`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | The repo's name. |

## `project`

Projects: what sessions work on and questions are about, each one or more repos (add, rm-repo, ls, show, bases, rebuild, hosts).

### `hotbed project rebuild NAME`

Build the project's next base from nothing on every worker. Bases are otherwise built incrementally: each generation forks the one before, pulls, and runs the steps that need to run.

*MCP tool: `project_rebuild`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `NAME` |  | The project. |

### `hotbed project hosts NAME [HOSTS...]`

Say which nodes keep the project's bases; none for every worker.

*MCP tool: `project_hosts`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `NAME` |  | The project. |
| `HOSTS` |  | Nodes (host names) that keep its bases; none for every worker. |

### `hotbed project add NAME`

Make a project, or add repos to one. Its workspaces check each repo out at /work/<repo>. A repo no project holds is a project of its own name, made when a session first starts in it. A repo another project holds needs --shared.

*MCP tool: `project_add` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `NAME` |  | Lowercase letters, digits and dashes. |
| `--repo` |  | A git remote it holds, in any form (git@github.com:o/r.git, https://github.com/o/r, github.com/o/r). Repeatable. |
| `--shared` |  | Add a repo another project holds too; sessions starting in its checkouts will then have to name their project (-p). |

### `hotbed project rm-repo NAME`

Take repos out of a project; sessions already in it stay.

*MCP tool: `project_rm-repo` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `NAME` |  | The project. |
| `--repo` |  | A git remote it holds, in any form (git@github.com:o/r.git, https://github.com/o/r, github.com/o/r). Repeatable. |

### `hotbed project ls`

List the projects, the repos each holds and their setup steps.

*MCP tool: `project_ls`*

### `hotbed project show NAME`

A project, with every change made to it: who made it, and who added or took out each repo, when.

*MCP tool: `project_show`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `NAME` |  | The project. |

### `hotbed project bases [PROJECT]`

List every node's bases for a project (or every project when none is named), newest generation first per node.

*MCP tool: `project_bases`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `PROJECT` |  | Project name (or a repo ID held by a project); empty lists every project's bases. |
| `--node` |  | Filter to bases on this node (host name or ID). |
| `--state` |  | Filter by base state (building, warm, stale, failed). |

## `freshen`

Inspect active and historical base freshens, phase/step timings, and latency bottlenecks (ls, show).

### `hotbed freshen ls [PROJECT]`

List active and historical base freshens with phase/step timing summaries, cold-build reasons, queue waits, and waiting workspaces.

*MCP tool: `freshen_ls`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `PROJECT` |  | Project name (or a repo ID held by a project); empty lists every project's freshens. |
| `--node` |  | Filter to freshens on this node (host name or ID). |
| `--state` |  | Filter by freshen state (queued, running, parked, paused, finalizing, succeeded, failed, cancelled, dropped). |
| `--active` |  | Only currently active (in-flight) freshens. |
| `--runtime` |  | Filter by runtime ("vm" or "cladefs"). |
| `--cold` |  | Only cold builds from nothing. |
| `--since` |  | Lookback window (e.g. "1h", "24h", "7d", or "all" for full retention; default "24h"). |
| `-n`, `--limit` |  | Maximum freshens to list (default 30; 0 for all). Summary covers all matched freshens in the window. |

### `hotbed freshen show ID`

Inspect one base freshen in detail: phase and step breakdown, cold reason, memory guarantee vs peak, waiting workspaces, timeline, and node admission.

*MCP tool: `freshen_show`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | Freshen command ID (e.g. f-cladefs-g75-831696) or base ID (e.g. cladefs-g75). |
| `--node` |  | Disambiguate when looking up by base ID across multiple nodes. |

## `snapshot`

### `hotbed snapshot`

Everything the cockpit shows: nodes, repos, bases, workspaces, commands, sessions, schedules, events and history.

*MCP tool: `snapshot`*

## `diagnostics`

### `hotbed diagnostics ID`

GetCommandDiagnostics is the diagnostics taken of a command as someone stopped or paused it (StopCommand, PauseCommand with diagnose), oldest first, live or long ended: the controller keeps them.

*MCP tool: `diagnostics`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | The command's ID. |
| `--seq` |  | Return only the diagnostics for this intervention sequence number (0 for all, paginated). |
| `--after-seq` |  | Return diagnostics with sequence number strictly greater than this cursor. |

## `samples`

The controller: membership, events, repos and the cockpit's view.

### `hotbed samples ls ID`

ListCommandSamples is the activity samples someone took of a command (SampleCommand), oldest first, without their contents: the controller keeps them, live or long ended. GetCommandSample has each one's.

*MCP tool: `samples_ls`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | The command's ID. |

### `hotbed samples show ID SEQ`

GetCommandSample is one activity sample someone took of a command, in full.

*MCP tool: `samples_show`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | The command's ID. |
| `SEQ` |  | Which of its samples: 1 for its first (hotbed samples ls). |

## `node`

Inspect or retire individual cluster worker nodes (show, retire).

### `hotbed node show NODE`

A node with how it decides whether to start new work (its admission: whether it holds work, why, and every signal it weighs against its limit), the work it recently held or made wait, and the placements that weighed it.

*MCP tool: `node_show`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `NODE` |  | Its host name or ID. |
| `--limit` |  | At most this many of its recent events and placements (default 30). |

### `hotbed node retire NODE`

Retire a node that is gone for good (its machine or VM deleted): it no longer shows in hotbed nodes or the cockpit and takes no runs, while its history (commands, events, placements) stays. Its sessions' runs go elsewhere. Refused while it is online. A worker joining again with the same node ID is a node again.

*MCP tool: `node_retire`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `NODE` |  | Its host name or ID. |

## `placements`

### `hotbed placements`

Placement decisions, newest first: each candidate node with its cost in parts, or why it was ruled out, and the node chosen. Kept a day.

*MCP tool: `placements`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `--node` |  | Only placements that weighed this node (host name or ID). |
| `--session` |  | Only this session's. |
| `--limit` |  | At most this many (default 50). |

## `events`

### `hotbed events`

The cluster's events, newest first: what workers did (admitted, held, made wait, resumed, parked, forked, ...), narrowed by node and kind.

*MCP tool: `events`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `--node` |  | Only this node's (host name or ID). |
| `--kind` |  | Only these kinds (repeatable): admit, wait, hold, unhold, park, thaw, fork, release, finish, fail, base, ... |
| `--limit` |  | At most this many (default 100). |

## `config`

Centrally managed cluster and per-node worker/VM configuration (show, set, rm).

### `hotbed config show [NODE]`

Read organization-scoped default and per-node worker/VM/freshen configuration.

*MCP tool: `config_show`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `NODE` |  | Worker hostname to inspect (including its merged effective config); omit for all scopes. |

### `hotbed config set [NODE]`

Set (merge or replace) organization default or per-node configuration (org admins only). Pass WorkerConfig JSON on stdin or --file.

*MCP tool: `config_set` · reads JSON from stdin or `--file`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `NODE` |  | Worker hostname to configure; omit to configure organization defaults. |
| `--replace` |  | Replace the scope's entire configuration instead of merging fields. |
| `--if-revision` |  | Refuse with CONFLICT unless the scope is at this revision (0 skips the check). |

### `hotbed config rm [NODE]`

Remove organization-scoped configuration overrides for a node (or organization defaults with --defaults). Org admins only.

*MCP tool: `config_rm`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `NODE` |  | Worker hostname whose overrides to delete; omit with --defaults to clear organization defaults. |
| `--defaults` |  | Delete organization defaults instead of a per-node override. |

## `system`

Explicit host system runtime operations such as upgrading cladefsd, firecracker, and guest kernels (upgrade).

### `hotbed system upgrade [NODE]`

Explicitly upgrade host cladefs components (cladefsd, firecracker, guest kernel) on one node or rolling across all online nodes one at a time. Stages and SHA-256-verifies artifacts before draining the node, stopping active commands/shells, releasing active cladefs/VM workspaces, installing binaries atomically, restarting cladefsd, and verifying health.

*MCP tool: `system_upgrade` · waits (no short deadline)*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `NODE` |  | Target worker hostname or ID; omit to roll across all online workers one at a time. |
| `--force` |  | Restart cladefsd even when installed binaries already match the configured SHA-256. |
| `--dry-run` |  | Stage and verify artifacts and report active work without terminating commands or restarting cladefsd. |

## `poll`

### `hotbed poll`

Wait, two minutes at most, for what happens in the session: its commands admitted, parked, paused or ended, its workspaces ready or failed, the user's answers to its questions, its reviews' ends and responses, and decisions on its proposed memories. Returns the events after the cursor at once if there are any. The client keeps where this machine's last poll of the session left off, so each event comes once. Nothing after two minutes: nothing happened yet; poll again.

*MCP tool: `poll` · uses `-s/--session` · waits (no short deadline)*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `--after` |  | Return events after this cursor (default: after the last ones a poll of this session on this machine returned; 0 for all of them). |
| `--timeout` |  | Seconds to wait for an event: at most and by default 120. |

## `ps`

### `hotbed ps`

List a session's commands, live and just ended (-p for a project, --all for every session, -e for 30-day history).

*MCP tool: `ps` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `-p`, `--project` |  | Every session's commands in this project. |
| `-e`, `--ended` |  | Commands that ended long ago too, as history keeps them (30 days), not only live and just-ended ones. |
| `-m`, `--match` |  | Only commands whose command line holds this text, as written (case-sensitive). |
| `-n`, `--limit` |  | At most this many of the commands that ended, the newest to end (default 50); 0 for every one. Commands not yet ended are all listed. |
| `--all` |  | Every session's commands. |

## `output`

### `hotbed output ID`

QueryOutput reads a command's output, live or ended, as its terminal shows it (settled: progress redraws collapsed to their last state, colors gone), or raw: the lines matching a pattern with context, a range of lines, its head or tail, or what came after a line already read; with until, it first waits for a matching line or the command's end. Every output is kept whole, so nothing need be filtered as it runs: read what matters once it does. Each answer is bounded and says what it left out.

*MCP tool: `output` · waits (no short deadline)*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | The command's ID. |
| `-g`, `--grep` |  | Only lines matching this RE2 regular expression, with --context lines around each. |
| `-i`, `--ignore-case` |  | Match grep and until ignoring case. |
| `-C`, `--context` |  | Lines of context around each grep match. |
| `--head` |  | Its first N lines. |
| `-n`, `--tail` |  | Its last N lines. |
| `--from` |  | Lines from this line number on (1 is the first). |
| `--to` |  | Lines up to and including this line number. |
| `--after` |  | Only lines after this line number: what came since an earlier answer's next line. |
| `--since-sync` |  | Only lines written since its workspace last synced: what a dev server logged since your last edit reached it. |
| `--until` |  | Wait, first, until a line (after --after) matches this RE2 regular expression or the command ends; then answer, the matching lines marked. |
| `--wait` |  | Wait, first, until the command ends. |
| `--timeout` |  | How long --until or --wait waits at most, in seconds (default 60, at most 600). |
| `--raw` |  | Lines as written, escape sequences and redraws included, not settled. |
| `--max` |  | At most this many lines in the answer (default 200, at most 5000). |
| `-t`, `--timestamps` |  | Prefix each line with the local time it was received (HH:MM:SS.mmm) in plain-text output. JSON output always includes received_at. |

## `detach`

### `hotbed detach ID`

DetachCommand lets an attached command run on with no one following it, as a detached one does: left to its hard time limit, its soft one gone. hotbed run does this when a command outlasts its --wait.

*MCP tool: `detach` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | The command's ID. |

## `send`

### `hotbed send ID TEXT`

SendInput types into a command run in a terminal (tty): a prompt's answer, a key a tool waits for.

*MCP tool: `send` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | The command's ID. |
| `TEXT` |  | What to type. A newline follows it unless --no-newline. |
| `--no-newline` |  | Send the text as it is, without a newline after it. |

## `daemon-logs`

### `hotbed daemon-logs`

GetDaemonLogs returns recent WARN and ERROR slog records from the controller, a worker, or all of them, newest first.

*MCP tool: `daemon-logs`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `--source` |  | Which daemon to read: "controller", a worker hostname or node ID, or empty for the controller and every online worker combined. |
| `--level` |  | Only entries of this level (warn or error); omit for both. |
| `-q`, `--query` |  | Case-insensitive substring filter over message, attributes, and source location. |
| `--from-ms` |  | Only entries at or after this unix ms timestamp (0 for no lower bound). |
| `--to-ms` |  | Only entries at or before this unix ms timestamp (0 for no upper bound). |
| `--since` |  | Only entries from the last duration (e.g. "15m", "1h", "24h", "7d"); sets from_ms relative to now on the server when from_ms is 0. |
| `-n`, `--limit` |  | Maximum entries to return, newest first (default 200, max 1000). |

## `schedule`

Scheduled commands: recurring work the controller starts by itself, such as a nightly measurement.

### `hotbed schedule add NAME [ARGS...]`

Schedule a command, or replace the scheduled command of that name. Each run gets a session of its own and a fresh workspace of the repo at its branch's head.

*MCP tool: `schedule_add`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `NAME` |  | Lowercase words joined by dashes, such as cladefs-nightly; each run's session is NAME-YYYYMMDD-HHMM. |
| `--cron` |  | When it runs: five-field cron (minute hour day-of-month month day-of-week) in the controller's time zone, such as '0 2 * * *'; CRON_TZ=ZONE in front for another zone. A slot that passes while the controller is down is missed, not run late, and a slot that comes while the previous run is in progress is skipped. |
| `-r`, `--repo` | `HOTBED_REPO` | The repo its runs work in (default: HOTBED_REPO, or the one this git checkout is of). |
| `--arch` |  | Puts every run on nodes of this architecture (amd64, arm64); otherwise the controller places each like any run (Placement). |
| `ARGS` |  | The command, after --; it runs at /work/REPO, as with hotbed run. |
| `--env` |  | KEY=VALUE for the command. Repeatable. |
| `--kind` |  | What it is, for history and build metrics: build, test, check, server or shell. |
| `--cpu-weight` |  | cgroup cpu.weight: the share of contended CPU (1-10000). |
| `--mem-low` |  | cgroup memory.low: guaranteed, such as 2G; admission waits until it fits. |
| `--mem-high` |  | cgroup memory.high, such as 8G: past it the command is throttled and swapped; 0 for none. |
| `--soft-limit` |  | How long it runs before it is paused unless it has ended, such as 10m, counted from its start and again from each time an agent starts waiting on it (FollowLog with wait); 0 for no soft limit. |
| `--hard-limit` |  | How long after its start it is stopped, paused or not, such as 2h; 0 for none. |
| `--priority` |  | Scheduling priority: low (-10), normal (0), high (10), urgent (20), or an integer from -100 to 100. Higher is admitted first; nothing running is preempted for it. |
| `--keep-idle` |  | Keep it running while it reserves over a quarter of the node's memory for work and keeps less than a core busy for each 6 GiB of it, which otherwise stops it after five minutes: for a command that rightly holds much memory for little CPU. |
| `--bench` |  | Run it as a measurement: alone among measurements on the node, once running work leaves it room, on CPUs no other command uses, isolated for its whole run, never swapped or parked. |
| `--vm-cpus` |  | vCPUs its VM's guest sees; 0 for the node's default. It sizes the VM only: admission goes by what commands are measured to use. vm runtime only. |
| `--vm-mem` |  | Memory its VM's guest sees, such as 16G; 0 for the node's default. It sizes the VM only, as vm_cpus does. vm runtime only. |
| `--paused` |  | Add it paused. |
| `-p`, `--project` | `HOTBED_PROJECT` | The project its runs' sessions work on: the one named when it was added (default: HOTBED_PROJECT), or else the one that holds its repo. |

### `hotbed schedule ls`

Scheduled commands, when each runs next, and their recent runs: the project's (default: HOTBED_PROJECT, the session's project, or the one that holds this checkout's repo), or every project's.

*MCP tool: `schedule_ls` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `-p`, `--project` | `HOTBED_PROJECT` | Only this project's (default: HOTBED_PROJECT, else the session's project, else the one holding this checkout's repo). |
| `--all-projects` |  | Every project's scheduled commands. |

### `hotbed schedule show NAME`

A scheduled command and its runs, each with its session (hotbed logs -s SESSION COMMAND).

*MCP tool: `schedule_show`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `NAME` |  | The scheduled command's name. |

### `hotbed schedule rm NAME`

Remove a scheduled command; a run in progress goes on.

*MCP tool: `schedule_rm`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `NAME` |  | The scheduled command's name. |

### `hotbed schedule run NAME`

Start a run now, outside the schedule, unless one is in progress; prints the run and its session.

*MCP tool: `schedule_run`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `NAME` |  | The scheduled command's name. |

### `hotbed schedule pause NAME`

Start no runs until resumed.

*MCP tool: `schedule_pause`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `NAME` |  | The scheduled command's name. |

### `hotbed schedule resume NAME`

Run again, from the next slot; the slots paused through are not made up.

*MCP tool: `schedule_resume`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `NAME` |  | The scheduled command's name. |

## `question`

Questions agents ask the user: a list for each project, shared by every session that works on it, which the user answers in the cockpit whenever they like.

### `hotbed question add`

Ask the user: AskUserQuestion's JSON ({"questions": [...]}) on stdin or --file. Each question is an entry of its own, at the top of the user's order, on the list of the session's project, which its sessions share; carry on with your best guess and read the answer later. With --answered, record decisions already taken instead: each question is answered with its implemented options and handled at once, for the user to reopen if they disagree.

*MCP tool: `question_add` · uses `-s/--session` · reads JSON from stdin or `--file`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `--repo` |  | The repo of the session's project they are about, as a git remote in any form (default: the checkout's asked from, if the project holds it, or else the project as a whole). |
| `--answered` |  | The questions record decisions the asking agent already took: each marks what it decided as implemented, and is kept answered with those options, by the asking session, and handled, never waiting in the outbox. The user reopens one to overturn it. |

### `hotbed question edit ID`

Rewrite an open question: one question's JSON on stdin or --file, bare or as add takes it.

*MCP tool: `question_edit` · uses `-s/--session` · reads JSON from stdin or `--file`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | The question's number. |
| `--if-revision` |  | Refuse with CONFLICT unless the question is at this revision. |

### `hotbed question rm ID`

Delete a question that is no longer needed, whichever session asked it; it stays in the history.

*MCP tool: `question_rm` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | The question's number. |
| `--reason` |  | Why it is no longer needed. |
| `--if-revision` |  | Refuse with CONFLICT unless the question is at this revision. |

### `hotbed question ls`

A project's questions, every session's (every state but deleted), newest first unless asked for another order.

*MCP tool: `question_ls` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `--state` |  | Only questions in this state (repeatable); none means every state but deleted. |
| `--sort` |  | newest or oldest asked first, the most recently changed (updated) first, or the user's order (rank). |
| `--asked-by` |  | Only the questions this session asked. |
| `--limit` |  | At most this many; 0 for all. |
| `-p`, `--project` | `HOTBED_PROJECT` | Only this project's questions (default: HOTBED_PROJECT, else the session's project, else the one holding this checkout's repo). |
| `--all-projects` |  | Every project's questions. |
| `--t3code-thread-id` |  | Only questions associated with this T3 Code thread ID (across every session of that thread). When set without -p/--project, --all-projects, -s, or a checkout remote, lists across every project. |

### `hotbed question outbox`

A project's questions the user answered or dismissed that no agent has acknowledged yet, the newest answer first, along with the asking sessions and their T3 Code threads.

*MCP tool: `question_outbox` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `--asked-by` |  | Only the questions this session asked. |
| `-p`, `--project` | `HOTBED_PROJECT` | Only this project's questions (default: HOTBED_PROJECT, else the session's project, else the one holding this checkout's repo). |
| `--all-projects` |  | Every project's questions. |
| `--t3code-thread-id` |  | Only questions associated with this T3 Code thread ID (across every session of that thread). When set without -p/--project, --all-projects, -s, or a checkout remote, lists across every project. |

### `hotbed question projects`

Every project with questions: how many wait on the user, and on agents.

*MCP tool: `question_projects`*

### `hotbed question show ID`

A question with every change made to it: what, when, and by which session.

*MCP tool: `question_show`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | The question's number. |

### `hotbed question answer ID`

Answer a question for the user, which puts it in the outbox; or change the answer of one in the outbox. The user answers in the cockpit; an agent uses this only to pass on what the user said.

*MCP tool: `question_answer` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | The question's number. |
| `--pick` |  | The label of an option the user picked; one at most unless the question is multi_select. |
| `--other` |  | What the user wrote instead of picking, or besides: AskUserQuestion's "Other". |
| `--notes` |  | A note on the answer. |
| `--if-revision` |  | Refuse with CONFLICT unless the question is at this revision. |

### `hotbed question dismiss ID`

Dismiss a question for the user, open or answered in the outbox: it will get no answer. It goes to the outbox too, so its asker learns as much.

*MCP tool: `question_dismiss` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | The question's number. |
| `--reason` |  | Why it gets no answer. |
| `--if-revision` |  | Refuse with CONFLICT unless the question is at this revision. |

### `hotbed question revise ID`

Rewrite a question for the user, in any state but deleted: the user does this in the cockpit; an agent only to pass on the user's words. An answer it has must still pick among its options. One question's JSON on stdin or --file, as question edit takes it.

*MCP tool: `question_revise` · uses `-s/--session` · reads JSON from stdin or `--file`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | The question's number. |
| `--if-revision` |  | Refuse with CONFLICT unless the question is at this revision. |

### `hotbed question reopen ID`

Return an answered, dismissed or handled question to the user, open again at the top of their order, its answer cleared: the user does this in the cockpit; an agent only when the user asks.

*MCP tool: `question_reopen` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | The question's number. |

### `hotbed question move ID`

Move an open question in the user's order.

*MCP tool: `question_move` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | The question's number. |
| `--before` |  | Just before this open question. |
| `--first` |  | First. |
| `--last` |  | Last. |

### `hotbed question ack [ID...]`

Acknowledge answers you have read and acted on: they leave the outbox, handled; all of them or, if one is not in the outbox, none.

*MCP tool: `question_ack` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | The questions whose answers you read. |

## `review`

Reviews of an exact version by reviewer agents.

### `hotbed review request`

Ask for a review of this checkout's exact version: from --base (default: where it left the remote's default branch) to --head (default: HEAD, or a snapshot of uncommitted changes), pinned under refs/hotbed/reviews so git keeps it. Names the implementer's exact model (--model, or inherited from the session). Prints the review; give its number to reviewer agent(s).

*MCP tool: `review_request` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `--title` |  | What it reviews, for people (default: the branch and commit). |
| `--instructions` |  | What the reviewer should look at, or for. |
| `--instructions-file` |  | Read `--instructions` from this file (`-` for stdin). |
| `--base` |  | The commit the changes are from: on the command line, a ref (default: where HEAD left the remote's default branch), which the client resolves. |
| `--head` |  | The commit to review: on the command line, a ref (default: HEAD, or, with uncommitted changes, a snapshot of them), which the client resolves and pins. |
| `--model` | `HOTBED_MODEL` | Exact model ID of the implementer (e.g. gemini-4-neon, claude-opus-4-6). Falls back to the implementer session's model if set; required if neither is set. |
| `--subagent` |  | Optional sub-agent label/role of the implementer when spawned inside a session. |
| `--implementer-session` |  | Optional implementer session ID if a different session requested the review on the implementer's behalf (defaults to session_id). |
| `--whole` |  | Review the whole tree at head, not its changes from a base. |

### `hotbed review ls`

A project's reviews (default: this session's project, or the one holding this checkout's repo), or every project's, newest first, with their findings counted.

*MCP tool: `review_ls` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `-p`, `--project` | `HOTBED_PROJECT` | Only this project's (default: HOTBED_PROJECT, else the session's project, else the one holding this checkout's repo). |
| `--all-projects` |  | Every project's reviews. |
| `--state` |  | Only reviews in this state (repeatable); none for all. |
| `--limit` |  | At most this many, newest first; 0 for all. |

### `hotbed review show ID`

A review: its version, implementer, reviewers, findings (with locations, decisions and responses) and every event.

*MCP tool: `review_show`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | The review's number. |

### `hotbed review link ID`

The review's page in the cockpit, to send the user.

*MCP tool: `review_link`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | The review's number. |

### `hotbed review start ID`

Reviewer: claim or join a review with your exact model (--model); several reviewers (and sub-agents in the same session) may review the same review.

*MCP tool: `review_start` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | The review's number. |
| `--model` |  | The exact model doing the review, as its provider names it (claude-opus-5-5, gpt-5.5-codex), not its family. |
| `--subagent` |  | Optional sub-agent label/role when running as a sub-agent inside a session (e.g. verifying-reviewer, codex-exec). |
| `--new` |  | Start a new reviewer pass even if an identical (session, model, subagent) reviewer is already in progress. |

### `hotbed review findings ID`

Reviewer: report findings, as ReportFindings takes them ({"findings": [...]}, each with file, line, summary, failure_scenario, category, severity, short_summary, verdict), with "locations": [{path, start_line, end_line, excerpt}] besides; {"findings": []} says you found none. JSON on stdin or --file, or --from-report REPORT: a reviewer's whole report ending with that JSON in a fenced json hotbed-findings block, which the review keeps. Call it as often as you like.

*MCP tool: `review_findings` · uses `-s/--session` · reads JSON from stdin or `--file`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | The review's number. |
| `--report` |  | Instead of findings, the reviewer's whole report (Markdown), which the review keeps: its findings are the last fenced block opened with ```json hotbed-findings, holding {"findings": [...]} as findings here. For a reviewer that cannot call hotbed itself, such as one in a network-isolated sandbox, whose caller passes its report on. |
| `--from-report` |  | Read `--report` from this file (`-` for stdin). |
| `--reviewer` |  | Which reviewer pass (1-based Reviewer.id) is reporting; optional when unambiguous for this session. |
| `--model` |  | Optional model filter to disambiguate multiple active reviewers in one session. |
| `--subagent` |  | Optional sub-agent filter to disambiguate multiple active reviewers in one session. |

### `hotbed review finish ID`

Reviewer: finish your review pass, with a summary.

*MCP tool: `review_finish` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | The review's number. |
| `--summary` |  | What you found, in a few lines. |
| `--reviewer` |  | Which reviewer pass (1-based Reviewer.id) is ending; optional when unambiguous for this session. |
| `--model` |  | Optional model filter to disambiguate multiple active reviewers in one session. |
| `--subagent` |  | Optional sub-agent filter to disambiguate multiple active reviewers in one session. |

### `hotbed review fail ID`

Reviewer: end your review pass as failed, saying why.

*MCP tool: `review_fail` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | The review's number. |
| `--reason` |  | Why the review could not be done. |
| `--reviewer` |  | Which reviewer pass (1-based Reviewer.id) is ending; optional when unambiguous for this session. |
| `--model` |  | Optional model filter to disambiguate multiple active reviewers in one session. |
| `--subagent` |  | Optional sub-agent filter to disambiguate multiple active reviewers in one session. |

### `hotbed review cancel ID`

Withdraw a review that has not ended.

*MCP tool: `review_cancel` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | The review's number. |
| `--reason` |  | Why it is no longer wanted. |
| `--model` | `HOTBED_MODEL` | Exact model cancelling the review (defaults to the session's or implementer's model). |
| `--subagent` |  | Optional sub-agent label. |

### `hotbed review accept ID FINDING...`

Accept findings: you will act on them.

*MCP tool: `review_accept` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | The review's number. |
| `FINDING` |  | The findings' numbers. |
| `--reason` |  | What you will do. |
| `--model` | `HOTBED_MODEL` | Exact model ID of the deciding agent (falls back to session/implementer model). |
| `--subagent` |  | Optional sub-agent label/role of the deciding agent. |
| `--as-user` |  | Explicitly mark USER vs AGENT when deciding from cockpit or CLI --as-user, which records the decision as the human user's rather than an agent session's. |

### `hotbed review dismiss ID FINDING...`

Dismiss (reject) findings: you will not act on them, saying why.

*MCP tool: `review_dismiss` · uses `-s/--session` · also `reject`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | The review's number. |
| `FINDING` |  | The findings' numbers. |
| `--reason` |  | Why you will not act on them. |
| `--model` | `HOTBED_MODEL` | Exact model ID of the deciding agent (falls back to session/implementer model). |
| `--subagent` |  | Optional sub-agent label/role of the deciding agent. |
| `--as-user` |  | Explicitly mark USER vs AGENT when deciding from cockpit or CLI --as-user, which records the decision as the human user's rather than an agent session's. |

### `hotbed review wont-fix ID FINDING...`

Mark findings won't fix: intentionally left unfixed, saying why.

*MCP tool: `review_wont-fix` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | The review's number. |
| `FINDING` |  | The findings' numbers. |
| `--reason` |  | Why you will not fix them. |
| `--model` | `HOTBED_MODEL` | Exact model ID of the deciding agent (falls back to session/implementer model). |
| `--subagent` |  | Optional sub-agent label/role of the deciding agent. |
| `--as-user` |  | Explicitly mark USER vs AGENT when deciding from cockpit or CLI --as-user, which records the decision as the human user's rather than an agent session's. |

### `hotbed review fixed ID FINDING...`

Mark findings fixed, in a commit (default: HEAD), with the files it changed and notes if you like. A fixed finding is handled: the user can respond to it no more.

*MCP tool: `review_fixed` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | The review's number. |
| `FINDING` |  | The findings' numbers. |
| `--commit` |  | The full commit that fixed it: 40 or 64 hex digits. On the command line, a ref resolved in this checkout (default: HEAD; outside one, give it in full). |
| `--where` |  | Where, in that commit; optional. On the command line, PATH, PATH:LINE or PATH:LINE-END. Repeatable. |
| `--notes` |  | What the fixing agent says of the fix, Markdown; optional. |
| `--notes-file` |  | Read `--notes` from this file (`-` for stdin). |
| `--model` | `HOTBED_MODEL` | Exact model ID of the deciding agent (falls back to session/implementer model). |
| `--subagent` |  | Optional sub-agent label/role of the deciding agent. |
| `--as-user` |  | Explicitly mark USER vs AGENT when deciding from cockpit or CLI --as-user, which records the decision as the human user's rather than an agent session's. |

### `hotbed review invalid ID FINDING...`

Mark findings invalid: not real defects, with the evidence (Markdown, citing commits, files and lines as you like). The user may still dispute it.

*MCP tool: `review_invalid` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | The review's number. |
| `FINDING` |  | The findings' numbers. |
| `--evidence` |  | Invalid: why it is not a real defect (Markdown): what you checked, citing commits, files and lines; given only then. |
| `--evidence-file` |  | Read `--evidence` from this file (`-` for stdin). |
| `--model` | `HOTBED_MODEL` | Exact model ID of the deciding agent (falls back to session/implementer model). |
| `--subagent` |  | Optional sub-agent label/role of the deciding agent. |
| `--as-user` |  | Explicitly mark USER vs AGENT when deciding from cockpit or CLI --as-user, which records the decision as the human user's rather than an agent session's. |

### `hotbed review reopen ID FINDING...`

Make decided findings (accepted, dismissed, won't fix, fixed or invalid) undecided again.

*MCP tool: `review_reopen` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | The review's number. |
| `FINDING` |  | The findings' numbers. |
| `--model` | `HOTBED_MODEL` | Exact model ID of the deciding agent (falls back to session/implementer model). |
| `--subagent` |  | Optional sub-agent label/role of the deciding agent. |
| `--as-user` |  | Explicitly mark USER vs AGENT when deciding from cockpit or CLI --as-user, which records the decision as the human user's rather than an agent session's. |

### `hotbed review respond ID`

Respond to a finding, or the review as a whole: a quick response, a comment, or both. Stamped as your bound agent session by default; pass --as-user to record a human user response.

*MCP tool: `review_respond` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | The review's number. |
| `--finding` |  | The finding it answers; 0 for the review as a whole. |
| `--quick` |  | A quick response to the finding. |
| `--comment` |  | What you say. |
| `--after` |  | When > 0, refuse with CONFLICT if the review already has events after this seq. |
| `--as-user` |  | Explicitly mark USER vs AGENT; if unspecified, inferred from session_id. --as-user records the response as the human user's, even with a session bound. |
| `--model` | `HOTBED_MODEL` | Exact model ID of the responding agent (falls back to session/implementer model). |
| `--subagent` |  | Optional sub-agent label/role of the responding agent. |

### `hotbed review responses ID`

The responses to a review since you last asked (or --after N; --all for every one), the user's above all.

*MCP tool: `review_responses`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | The review's number. |
| `--after` |  | Events after this cursor, 0 for all (default: after the last ones this machine read). |
| `--all` |  | Every response, from the first. |

## `auth`

Centralized GitHub authentication held on the controller and delegated to every worker, session, and command (git HTTPS credential helper, git SSH protocol proxy, and worker gh REST/GraphQL proxy).

### `hotbed auth login-start`

Start a GitHub OAuth Device Flow on the controller and return the one-time user code and verification URL (use with `hotbed auth login --login-id`, or run `hotbed auth login` directly for the interactive flow).

*MCP tool: `auth_login-start`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `--name` |  | Credential name to save on completion (default "github"). |
| `--host` |  | GitHub host (default "github.com"). |
| `--repo` |  | Optional repo patterns ("owner/repo" or "owner/*") this credential covers. |
| `--scope` |  | Additional OAuth scopes beyond repo, read:org, gist. |
| `--client-id` |  | OAuth App client ID override (default: Hotbed's public OAuth App). |
| `--api-url` |  | Optional GitHub REST API base URL override. |
| `--oauth-url` |  | Optional GitHub OAuth base URL override (default https://<host>). |

### `hotbed auth login`

Log in to GitHub on the controller via OAuth Device Flow (like `gh auth login`) and store the resulting user token centrally for all workers, sessions, and commands.

*MCP tool: `auth_login` · waits (no short deadline)*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `--login-id` |  | Pending login handle from `auth login-start` (omit for interactive login). |
| `--name` |  | Credential name to save (local: used when starting interactive login). |
| `--host` |  | GitHub host (local: used when starting interactive login). |
| `--repo` |  | Optional repo patterns (local: used when starting interactive login). |
| `--scope` |  | Additional OAuth scopes (local: used when starting interactive login). |
| `--client-id` |  | OAuth App client ID override (local: used when starting interactive login). |
| `--api-url` |  | Optional API base URL override (local: used when starting interactive login). |
| `--oauth-url` |  | Optional OAuth base URL override (local: used when starting interactive login). |
| `-w`, `--web` |  | Open the verification URL in your browser automatically. |

### `hotbed auth set NAME`

Add or replace a centralized git token on the controller (for headless or CI setup; prefer `hotbed auth login` for interactive OAuth login). Secrets are stored write-only on the controller and never returned by list calls.

*MCP tool: `auth_set`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `NAME` |  | Unique credential name (lowercase letters, digits, dashes), e.g. github. |
| `--host` |  | Git host (default github.com). |
| `--repo` |  | Optional repo patterns ("owner/repo" or "owner/*") this credential covers; repeatable; omit for all repos on host. |
| `--token` |  | Access token (prefer --token-file - to read from stdin, which keeps it out of argv). |
| `--token-file` |  | Read `--token` from this file (`-` for stdin). |
| `--username` |  | Optional GitHub login name for display in `auth ls` and `gh auth status`. |
| `--api-url` |  | Optional GitHub REST/GraphQL API base URL override (default https://api.github.com). |

### `hotbed auth ls`

List centralized git credentials configured on the controller (metadata only; secrets are never returned).

*MCP tool: `auth_ls`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `--host` |  | Only credentials for this git host; empty for all. |

### `hotbed auth rm NAME`

Delete a centralized git credential from the controller.

*MCP tool: `auth_rm`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `NAME` |  | Name of the credential to delete. |

### `hotbed auth token REPO`

Mint a short-lived git access token for a repository URL or canonical remote (e.g. https://github.com/semistrict/hotbed.git), refreshing an expiring OAuth token automatically when needed.

*MCP tool: `auth_token`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `REPO` |  | Repository URL or canonical remote, e.g. https://github.com/semistrict/hotbed.git or github.com/semistrict/hotbed. |

### `hotbed auth api PATH`

Proxy a GitHub REST or GraphQL API request through the controller using its stored credentials so worker commands (`gh`) never need raw tokens.

*MCP tool: `auth_api`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `PATH` |  | API path starting with "/", e.g. /user, /repos/semistrict/hotbed/pulls, or /graphql. |
| `-X`, `--method` |  | HTTP method (default GET, or POST when body is set). |
| `--host` |  | GitHub host (default github.com). |
| `--repo` |  | Optional "owner/repo" hint for repo-scoped credential selection. |
| `--body` |  | Optional request body (e.g. JSON for POST/PATCH/GraphQL). |
| `-H`, `--header` |  | Additional request headers as "Key: Value" (Authorization/Cookie are ignored). |

## `issue`

Cross-agent memories and learned rules shared across sessions and machines.

### `hotbed issue report TITLE`

Report a problem with hotbed itself: what you did, what happened, what you expected, and the command or error. List first (hotbed issue ls -q WORD): one already reported is +1'd (hotbed issue upvote), not reported again.

*MCP tool: `issue_report` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `TITLE` |  | One line: what is wrong. |
| `-m`, `--body` |  | What you did, what happened, what you expected; the command and error. |

### `hotbed issue ls`

List the issues reported, most +1'd first: open ones, or archived ones, or all; -q narrows them to those whose title or body holds the words.

*MCP tool: `issue_ls`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `--archived` |  | Archived issues instead of open ones. |
| `--all` |  | Open and archived issues alike. |
| `-q`, `--query` |  | Only issues whose title or body holds each of these words (any case). |

### `hotbed issue show ID`

Show an issue: what was reported, by whom, and everyone who +1'd it.

*MCP tool: `issue_show`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  |  |

### `hotbed issue upvote ID`

+1 an issue you hit too, saying what you saw if it adds anything. Each session +1s an issue once; its reporter's report counts as one.

*MCP tool: `issue_upvote` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  |  |
| `-m`, `--comment` |  | What you saw, if it adds to the report. |

### `hotbed issue archive ID`

Archive an issue dealt with (fixed, or not to be): the user does this in the cockpit; an agent only when the user asks.

*MCP tool: `issue_archive` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  |  |
| `-m`, `--reason` |  | Why: fixed (say where), or not to be fixed (say why). |

### `hotbed issue reopen ID`

Reopen an archived issue: it is back.

*MCP tool: `issue_reopen` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  |  |

## `memory`

### `hotbed memory add`

Save a new memory or supersede/consolidate existing memories (--supersede ID). Automatically approved and active immediately. Defaults to the active session's project; pass --global for cross-project learnings. When --mode is omitted, omitting --body saves a short always-on rule (<= 240 chars) and passing --body/--body-file saves an on-demand playbook fetched with `hotbed memory get`.

*MCP tool: `memory_add` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `--slug` |  | Lowercase kebab-case identifier, e.g. "subagent-model-neon" (max 64 chars). |
| `--title` |  | Short human title (optional; max 120 chars). |
| `--summary` |  | Compact in-context component: the complete rule for ALWAYS mode (<= 240 chars), or a 1-line description + when-to-read trigger for ON_DEMAND mode (<= 280 chars). |
| `--body` |  | Full markdown details fetched via `hotbed memory get` (required for ON_DEMAND, optional for ALWAYS; max 64 KiB). |
| `--body-file` |  | Read `--body` from this file (`-` for stdin). |
| `--mode` |  | Delivery mode: always (inline rule on session start) or on_demand (index on session start, full body fetched on demand). |
| `--tag` |  | Short lowercase tags for filtering and search (repeatable). |
| `--global` |  | Propose as a global memory (applies across all projects and machines). |
| `-p`, `--project` |  | Project to scope this memory to (default: the active session's project). |
| `--repo` |  | Optional repo within the project (default: this checkout's remote if held by the project). |
| `--supersede` |  | Existing approved memory IDs this proposal updates or consolidates (repeatable). |
| `--rationale` |  | Why this learning is being added and what triggered it (required for agents; stored as metadata only, not returned when reading memories in context). |
| `--model` |  | Exact model ID proposing this memory (optional). |

### `hotbed memory propose`

Alias for `hotbed memory add`.

*MCP tool: `memory_propose` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `--slug` |  | Lowercase kebab-case identifier, e.g. "subagent-model-neon" (max 64 chars). |
| `--title` |  | Short human title (optional; max 120 chars). |
| `--summary` |  | Compact in-context component: the complete rule for ALWAYS mode (<= 240 chars), or a 1-line description + when-to-read trigger for ON_DEMAND mode (<= 280 chars). |
| `--body` |  | Full markdown details fetched via `hotbed memory get` (required for ON_DEMAND, optional for ALWAYS; max 64 KiB). |
| `--body-file` |  | Read `--body` from this file (`-` for stdin). |
| `--mode` |  | Delivery mode: always (inline rule on session start) or on_demand (index on session start, full body fetched on demand). |
| `--tag` |  | Short lowercase tags for filtering and search (repeatable). |
| `--global` |  | Propose as a global memory (applies across all projects and machines). |
| `-p`, `--project` |  | Project to scope this memory to (default: the active session's project). |
| `--repo` |  | Optional repo within the project (default: this checkout's remote if held by the project). |
| `--supersede` |  | Existing approved memory IDs this proposal updates or consolidates (repeatable). |
| `--rationale` |  | Why this learning is being added and what triggered it (required for agents; stored as metadata only, not returned when reading memories in context). |
| `--model` |  | Exact model ID proposing this memory (optional). |

### `hotbed memory ls`

List memories (compact metadata without full body). Defaults to approved memories for global + active project (or global only outside a project).

*MCP tool: `memory_ls` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `--state` |  | Filter by state (repeatable); default is approved. |
| `--scope` |  | Filter by scope: global or project; omit for global + active project. |
| `--mode` |  | Filter by delivery mode: always or on_demand. |
| `-p`, `--project` | `HOTBED_PROJECT` | Only memories for this project (with global unless --scope project). |
| `--all-projects` |  | List memories across global and every project. |
| `--tag` |  | Filter by tag. |
| `-q`, `--query` |  | Case-insensitive substring search across slug, title, summary, tags, and body. |
| `--sort` |  | Sort order: rank, newest, oldest, updated, or slug. |
| `-n`, `--limit` |  | Maximum memories to return; 0 for all. |

### `hotbed memory get REF...`

Fetch the full markdown body of one or more memories by slug or ID (e.g. `subagent-model-neon`, `global/subagent-model-neon`, `hotbed/slug`, or `12`).

*MCP tool: `memory_get` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `REF` |  | Memory IDs or slugs (e.g. 12, subagent-model-neon, global/slug, hotbed/slug). |
| `-p`, `--project` | `HOTBED_PROJECT` | Project to resolve bare slugs against (default: active session or checkout project). |

### `hotbed memory show REF`

Show one memory with its full body, provenance, supersession links, and complete revision history.

*MCP tool: `memory_show` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `REF` |  | Memory ID or slug (e.g. 12, subagent-model-neon, global/slug, hotbed/slug). |
| `-p`, `--project` | `HOTBED_PROJECT` | Project to resolve a bare slug against. |

### `hotbed memory edit ID`

Edit a memory in any way (slug, title, summary, body, mode, tags, scope, repo, or rationale). Unspecified fields stay unchanged unless --replace-content or a clear flag is passed.

*MCP tool: `memory_edit` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | Memory ID to edit. |
| `--slug` |  | Lowercase kebab-case identifier, e.g. "subagent-model-neon" (max 64 chars). |
| `--title` |  | Short human title (optional; max 120 chars). |
| `--summary` |  | Compact in-context component: the complete rule for ALWAYS mode (<= 240 chars), or a 1-line description + when-to-read trigger for ON_DEMAND mode (<= 280 chars). |
| `--body` |  | Full markdown details fetched via `hotbed memory get` (required for ON_DEMAND, optional for ALWAYS; max 64 KiB). |
| `--body-file` |  | Read `--body` from this file (`-` for stdin). |
| `--mode` |  | Delivery mode: always (inline rule on session start) or on_demand (index on session start, full body fetched on demand). |
| `--tag` |  | Short lowercase tags for filtering and search (repeatable). |
| `--global` |  | Change scope to global (shared across all projects). |
| `-p`, `--project` |  | Change scope to this project. |
| `--if-revision` |  | Refuse with CONFLICT unless the memory is at this revision. |
| `--replace-content` |  | Replace all content fields (slug, title, summary, body, mode, tags) with the provided content instead of merging only non-empty fields. |
| `--clear-title` |  | Clear the optional title. |
| `--clear-body` |  | Clear the optional body (allowed in always mode). |
| `--replace-tags` |  | Replace tags with the provided --tag list (omit --tag to clear all tags). |
| `--repo` |  | Set or clear ("") the optional repo within a project-scoped memory. |
| `--rationale` |  | Set or clear ("") the provenance rationale. |

### `hotbed memory promote ID`

Promote a project-specific memory to a global memory shared across all projects.

*MCP tool: `memory_promote` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | Memory ID to edit. |
| `--slug` |  | Lowercase kebab-case identifier, e.g. "subagent-model-neon" (max 64 chars). |
| `--title` |  | Short human title (optional; max 120 chars). |
| `--summary` |  | Compact in-context component: the complete rule for ALWAYS mode (<= 240 chars), or a 1-line description + when-to-read trigger for ON_DEMAND mode (<= 280 chars). |
| `--body` |  | Full markdown details fetched via `hotbed memory get` (required for ON_DEMAND, optional for ALWAYS; max 64 KiB). |
| `--body-file` |  | Read `--body` from this file (`-` for stdin). |
| `--mode` |  | Delivery mode: always (inline rule on session start) or on_demand (index on session start, full body fetched on demand). |
| `--tag` |  | Short lowercase tags for filtering and search (repeatable). |
| `--if-revision` |  | Refuse with CONFLICT unless the memory is at this revision. |
| `--replace-content` |  | Replace all content fields (slug, title, summary, body, mode, tags) with the provided content instead of merging only non-empty fields. |
| `--clear-title` |  | Clear the optional title. |
| `--clear-body` |  | Clear the optional body (allowed in always mode). |
| `--replace-tags` |  | Replace tags with the provided --tag list (omit --tag to clear all tags). |
| `--repo` |  | Set or clear ("") the optional repo within a project-scoped memory. |
| `--rationale` |  | Set or clear ("") the provenance rationale. |

### `hotbed memory archive ID`

Archive an active memory so it is no longer returned on session start; history is preserved and it can be restored.

*MCP tool: `memory_archive` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | Active memory ID to archive. |
| `--reason` |  | Why it is being archived (required for agents). |
| `--if-revision` |  |  |

### `hotbed memory restore ID`

Restore an archived, rejected, or withdrawn memory back to active.

*MCP tool: `memory_restore` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | Archived, rejected, or withdrawn memory ID to restore. |
| `--if-revision` |  |  |

### `hotbed memory rm ID`

Permanently remove a memory and its revision history.

*MCP tool: `memory_rm` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | Memory ID to permanently remove. |
| `--if-revision` |  | Refuse with CONFLICT unless the memory is at this revision. |

### `hotbed memory move ID`

Move an approved memory in the user's startup priority order.

*MCP tool: `memory_move` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | Approved memory ID to move. |
| `--before` |  | Just before this approved memory. |
| `--first` |  | First. |
| `--last` |  | Last. |
