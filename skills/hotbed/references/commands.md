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

Retire a node that is gone for good (its machine or VM deleted): it no longer shows in hotbed nodes or the cockpit and takes no runs, while its history (commands, events, placements) stays. Its sessions' runs go elsewhere. Refused while it is online. A worker joining again with the same tailnet identity is a node again.

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

Read controller-managed cluster default and per-node worker/VM configuration.

*MCP tool: `config_show`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `NODE` |  | Worker hostname to inspect (including its merged effective config); omit for all scopes. |

### `hotbed config set [NODE]`

Set (merge or replace) controller-managed cluster default or per-node worker/VM configuration. Pass WorkerConfig JSON on stdin or --file.

*MCP tool: `config_set` · reads JSON from stdin or `--file`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `NODE` |  | Worker hostname to configure; omit to configure cluster-wide defaults. |
| `--replace` |  | Replace the scope's entire configuration instead of merging fields. |
| `--if-revision` |  | Refuse with CONFLICT unless the scope is at this revision (0 skips the check). |

### `hotbed config rm [NODE]`

Remove controller-managed worker/VM configuration for a node (or cluster defaults with --defaults).

*MCP tool: `config_rm`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `NODE` |  | Worker hostname whose overrides to delete; omit with --defaults to clear cluster defaults. |
| `--defaults` |  | Delete cluster-wide defaults instead of a per-node override. |

## `system`

Explicit host system runtime operations such as upgrading cladefsd, firecracker, and guest kernels (upgrade).

### `hotbed system upgrade [NODE]`

Explicitly upgrade host cladefs components (cladefsd, firecracker, guest kernel) on one node or rolling across all online nodes one at a time. Stages and SHA-256-verifies artifacts before draining the node, stopping active commands/shells, releasing active cladefs/VM workspaces, installing binaries atomically, restarting cladefsd, and verifying health.

*MCP tool: `system_upgrade`*

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
| `--token` |  | Access token (prefer --token-file - to read from stdin). |
| `--token-file` |  | Path to a file holding the token, or "-" to read from stdin. |
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

## `memory`

Cross-agent memories and learned rules curated with user approval.

### `hotbed memory propose`

Propose a new memory or an update/consolidation of existing approved memories (--supersede ID). Defaults to the active session's project; pass --global for cross-project learnings. Use --mode always for short guardrails (<= 240 chars) or --mode on_demand (default) for detailed playbooks fetched with `hotbed memory get`.

*MCP tool: `memory_propose` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `--slug` |  | Lowercase kebab-case identifier, e.g. "subagent-model-neon" (max 64 chars). |
| `--title` |  | Short human title (optional; max 120 chars). |
| `--summary` |  | Compact in-context component: the complete rule for ALWAYS mode (<= 240 chars), or a 1-line description + when-to-read trigger for ON_DEMAND mode (<= 280 chars). |
| `--body` |  | Full markdown details fetched via `hotbed memory get` (required for ON_DEMAND, optional for ALWAYS; max 64 KiB). |
| `--mode` |  | Delivery mode: always (inline rule on session start) or on_demand (index on session start, full body fetched on demand). |
| `--tag` |  | Short lowercase tags for filtering and search (repeatable). |
| `--global` |  | Propose as a global memory (applies across all projects and machines). |
| `-p`, `--project` |  | Project to scope this memory to (default: the active session's project). |
| `--repo` |  | Optional repo within the project (default: this checkout's remote if held by the project). |
| `--supersede` |  | Existing approved memory IDs this proposal updates or consolidates (repeatable). |
| `--rationale` |  | Why this learning is being proposed (e.g. user correction or incident). |
| `--model` |  | Exact model ID proposing this memory (optional). |
| `--approve` |  | Directly approve on creation (user-only: refused when called from an agent session). |
| `--body-file` |  | Path to a markdown file for body, or "-" to read from stdin (local). |

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

### `hotbed memory proposals`

List proposed memories waiting for the user's approval, newest first.

*MCP tool: `memory_proposals` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `--scope` |  | Filter by scope: global or project; omit for global + active project. |
| `--mode` |  | Filter by delivery mode: always or on_demand. |
| `-p`, `--project` | `HOTBED_PROJECT` | Only memories for this project (with global unless --scope project). |
| `--all-projects` |  | List memories across global and every project. |
| `--tag` |  | Filter by tag. |
| `-q`, `--query` |  | Case-insensitive substring search across slug, title, summary, tags, and body. |
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

Edit a proposed memory before user approval (agent action).

*MCP tool: `memory_edit` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | Memory ID to edit (must be in PROPOSED state). |
| `--slug` |  | Lowercase kebab-case identifier, e.g. "subagent-model-neon" (max 64 chars). |
| `--title` |  | Short human title (optional; max 120 chars). |
| `--summary` |  | Compact in-context component: the complete rule for ALWAYS mode (<= 240 chars), or a 1-line description + when-to-read trigger for ON_DEMAND mode (<= 280 chars). |
| `--body` |  | Full markdown details fetched via `hotbed memory get` (required for ON_DEMAND, optional for ALWAYS; max 64 KiB). |
| `--mode` |  | Delivery mode: always (inline rule on session start) or on_demand (index on session start, full body fetched on demand). |
| `--tag` |  | Short lowercase tags for filtering and search (repeatable). |
| `--rationale` |  | Optional updated rationale. |
| `--supersede` |  | Updated list of superseded approved memory IDs (pass replace_supersedes to clear). |
| `--replace-supersedes` |  | Replace supersedes_ids with the given list (even if empty). |
| `--if-revision` |  | Refuse with CONFLICT unless the memory is at this revision. |
| `--body-file` |  | Path to a markdown file for body, or "-" to read from stdin (local). |

### `hotbed memory withdraw ID`

Withdraw a proposed memory that is no longer needed; it stays in history.

*MCP tool: `memory_withdraw` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | Memory ID to withdraw (must be in PROPOSED state). |
| `--reason` |  | Why the proposal is being withdrawn. |
| `--if-revision` |  |  |

### `hotbed memory approve ID`

Approve a proposed memory for the user, optionally applying edits first. Any approved memories listed in supersedes_ids are atomically superseded. The user approves in the cockpit; an agent uses this only at the user's word.

*MCP tool: `memory_approve` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | Proposed memory ID to approve. |
| `--slug` |  | Lowercase kebab-case identifier, e.g. "subagent-model-neon" (max 64 chars). |
| `--title` |  | Short human title (optional; max 120 chars). |
| `--summary` |  | Compact in-context component: the complete rule for ALWAYS mode (<= 240 chars), or a 1-line description + when-to-read trigger for ON_DEMAND mode (<= 280 chars). |
| `--body` |  | Full markdown details fetched via `hotbed memory get` (required for ON_DEMAND, optional for ALWAYS; max 64 KiB). |
| `--mode` |  | Delivery mode: always (inline rule on session start) or on_demand (index on session start, full body fetched on demand). |
| `--tag` |  | Short lowercase tags for filtering and search (repeatable). |
| `--global` |  | Optionally change scope to global on approval. |
| `-p`, `--project` |  | Optionally change scope to this project on approval. |
| `--note` |  | Optional note on approval. |
| `--if-revision` |  |  |
| `--body-file` |  | Path to a markdown file for body, or "-" to read from stdin (local). |

### `hotbed memory reject ID`

Reject a proposed memory for the user with an optional reason.

*MCP tool: `memory_reject` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | Proposed memory ID to reject. |
| `--reason` |  | Why it was rejected (delivered to the proposing session via poll). |
| `--if-revision` |  |  |

### `hotbed memory revise ID`

Revise an approved or proposed memory directly for the user.

*MCP tool: `memory_revise` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | Memory ID to revise (APPROVED or PROPOSED). |
| `--slug` |  | Lowercase kebab-case identifier, e.g. "subagent-model-neon" (max 64 chars). |
| `--title` |  | Short human title (optional; max 120 chars). |
| `--summary` |  | Compact in-context component: the complete rule for ALWAYS mode (<= 240 chars), or a 1-line description + when-to-read trigger for ON_DEMAND mode (<= 280 chars). |
| `--body` |  | Full markdown details fetched via `hotbed memory get` (required for ON_DEMAND, optional for ALWAYS; max 64 KiB). |
| `--mode` |  | Delivery mode: always (inline rule on session start) or on_demand (index on session start, full body fetched on demand). |
| `--tag` |  | Short lowercase tags for filtering and search (repeatable). |
| `--global` |  | Optionally change scope to global. |
| `-p`, `--project` |  | Optionally change scope to this project. |
| `--if-revision` |  |  |
| `--body-file` |  | Path to a markdown file for body, or "-" to read from stdin (local). |

### `hotbed memory archive ID`

Archive an approved memory so it is no longer returned on session start; history is preserved and it can be restored.

*MCP tool: `memory_archive` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | Approved memory ID to archive. |
| `--reason` |  | Why it was archived. |
| `--if-revision` |  |  |

### `hotbed memory restore ID`

Restore an archived or rejected memory (to approved, or back to proposed).

*MCP tool: `memory_restore` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | Archived or rejected memory ID to restore. |
| `--to-proposed` |  | Restore back to PROPOSED instead of APPROVED. |
| `--if-revision` |  |  |

### `hotbed memory move ID`

Move an approved memory in the user's startup priority order.

*MCP tool: `memory_move` · uses `-s/--session`*

| Argument / Flag | Env | Help |
| --- | --- | --- |
| `ID` |  | Approved memory ID to move. |
| `--before` |  | Just before this approved memory. |
| `--first` |  | First. |
| `--last` |  | Last. |
