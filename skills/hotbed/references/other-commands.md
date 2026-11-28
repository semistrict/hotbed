# Other commands

| Command | Does |
|---|---|
| `hotbed session status [--t3code-thread-id ID]` | Active session state, idle time, workspaces, and commands (`--t3code-thread-id`: look up the live or latest session for a T3 Code thread on the controller) |
| `hotbed session current` | Active session and where it came from (`flag`, `env`, `thread`, `worktree`) |
| `hotbed session ls [--ended] [--all-projects] [--t3code-thread-id ID]` | The project's live sessions (`--ended`, `--all-projects`, `--t3code-thread-id`) |
| `hotbed session search [THREAD_ID] [--t3code-thread-id ID] [--live] [-p PROJECT] [-n N]` | Search persistently across every project (and ended sessions of any age) for sessions associated with a T3 Code thread ID (default: `T3CODE_THREAD_ID`), live ones first then most recently active |
| `hotbed shell [--arch ARCH] [--runtime RT] [--sync] [--env K=V] [-- CMD...]` | Open an interactive shell (over SSH) in the session's workspace (reuses an existing workspace when it has one; `--sync` pushes local checkout edits first) |
| `hotbed project ls` | Projects, their repos, live sessions, schedules, reviews and questions |
| `hotbed project show NAME` | A project and every change to it |
| `hotbed project bases [PROJECT] [--node HOST] [--state STATE]` | Every node's bases for a project (or every project), newest generation first per node, with commits, freshen command ID, and failed step |
| `hotbed freshen ls [PROJECT] [--node HOST] [--state STATE] [--active] [--runtime RT] [--cold] [--since 24h] [-n N]` | Active and historical (30-day) base freshens across all runtimes (`vm`, `cladefs`), with cluster-wide timing breakdowns (prep, queue, git, steps, parked, finalize/R2-sync), slowest steps ranking, per-node/project cold & failure streaks, and waiting workspaces |
| `hotbed freshen show ID [--node HOST]` | Deep autopsy of one base freshen (by freshen command ID or base ID): cold reason, phase & step timings, memory guarantee vs peak, waiting workspaces, command timeline, live node admission snapshot, and related events |
| `hotbed project rm-repo NAME --repo REMOTE` | Take a repo out of a project |
| `hotbed schedule add ... -p PROJECT` | A schedule's runs work on its project (default: the one holding its repo) |
| `hotbed schedule ls [-p PROJECT \| --all-projects]` | The project's schedules (`--all-projects`: every project's) |
| `hotbed ps [-p PROJECT \| --all] [-e] [-m TEXT] [-n N]` | The session's commands, live and just ended (`-p`: a project's, `--all`: every session's; `-e`: ended ones too, from 30 days of history; `-m`: only those whose command line holds TEXT; `-n`: the newest N ended ones, default 50, 0 for all; running ones always show) |
| `hotbed output CMD [--grep P -C N] [--head N \| --tail N] [--from N --to M] [--after N] [--until P \| --wait] [--timeout S] [--since-sync] [--raw] [--max N] [-t \| --timestamps]` | Read a command's output, kept whole, as its terminal shows it: bounded answers that say what they left out; `--until` / `--wait` wait first; `-t`/`--timestamps` prefixes each line with its local received time (`HH:MM:SS.mmm`, always present as `receivedAt` in `--json`) (MCP tool `output`) |
| `hotbed detach CMD` | Let an attached command run on, detached (MCP tool `detach`) |
| `hotbed send CMD TEXT [--no-newline]` | Type into a command run with `--tty` (MCP tool `send`) |
| `hotbed issue report TITLE [-m DETAILS]` / `issue ls [-q WORDS] [--archived \| --all]` / `issue show ID` / `issue upvote ID [-m COMMENT]` / `issue archive ID [-m WHY]` / `issue reopen ID` | Problems with hotbed itself: report one once, +1 it after (MCP tools `issue_report`, `issue_ls`, …) |
| `hotbed logs CMD [-f \| -n N] [-t \| --timestamps]` | A command's output, any session's by its ID; `-f` (the session's own command: it waits on it) follows until it ends, `-n N` its last N lines (at most 1 MiB of them; stderr says when it cut), `-t`/`--timestamps` prefixes each line with its local received time (`HH:MM:SS.mmm`). A log keeps its first 16 MiB and last 240 MiB, with a `[hotbed: … dropped here …]` line between; logs of ended commands are kept 7 days |
| `hotbed stop CMD [-m COMMENT] [--diagnose]` | Stops a command of the session (another session's: `-s SESSION`); with `--diagnose`, takes its diagnostics first |
| `hotbed pause CMD [-m COMMENT] [--diagnose]` | Pauses a command by hand: frozen until `hotbed resume`, waiting on it or not; with `--diagnose`, takes its diagnostics first |
| `hotbed resume CMD [-m COMMENT]` | Resumes a paused command |
| `hotbed start CMD [-m COMMENT]` | Starts a queued command now, past the node's memory, CPU and disk holds |
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
| `hotbed nodes` | The cluster's machines, and the build each runs |
| `hotbed node show HOST` | Whether the node starts new work or holds it, and why: each signal (CPU pressure, memory pressure, memory reserved, disk free) against its limit, the work it held or made wait, and the placements that weighed it |
| `hotbed node retire HOST` | Retire a node gone for good (its machine or VM deleted): it leaves `hotbed nodes`, the cockpit and placement; its history stays. Refused while it is online |
| `hotbed config show [HOST]` | Show controller-managed cluster default and per-node worker/VM configuration (plus merged effective config when `HOST` is given) |
| `hotbed config set [HOST] [--replace] [--if-revision N]` | Merge (or replace) controller-managed cluster defaults or per-node worker/VM configuration (VM tuning, managed guest kernels, `hotbed-guestd`, FUSE settings) from JSON on stdin or `--file` |
| `hotbed config rm HOST \| --defaults` | Remove controller-managed per-node overrides for `HOST`, or clear cluster-wide defaults with `--defaults` |
| `hotbed placements [--node HOST] [--session S]` | Where the controller put each workspace and why: what the run asked for (`--arch`, `--runtime`, `--mem-low`, `--mem`, `--fork`, `--no-sync`, or `any node`), every node's cost in parts, or why it was ruled out (kept a day; also recorded on each command in `ps --json` and the cockpit) |
| `hotbed events [--node HOST] [--kind KIND ...]` | The cluster's events, newest first; `--kind hold --kind wait --kind unhold` is every time a node held work and why |
| `hotbed daemon-logs [--source controller\|HOST] [--level warn\|error] [--since 1h] [-q Q] [-n N]` | Rotating WARN/ERROR slog logs from the controller and every worker, newest first (with rotation disk stats) |
| `hotbed snapshot` | Everything the cockpit shows, as JSON |
| `hotbed upgrade` | Install the controller's build over this hotbed (a CI build does it by itself) |
| `hotbed system upgrade [HOST] [--force] [--dry-run]` | Explicitly upgrade host `cladefsd`, `firecracker`, and managed guest kernel binaries from controller config (`system.cladefsd`, `system.firecracker`, `vm.kernels`); drains and terminates live commands, shells, and workspaces on each node before restarting `cladefsd` |
| `hotbed login [CONTROLLER] [--org ORG] [--no-open]` | Sign in to a WorkOS-enabled controller via one-time device code (works identically locally and over SSH, no localhost callback) |
| `hotbed logout [CONTROLLER]` | Remove locally stored WorkOS credentials for a controller |
| `hotbed whoami` | Show the signed-in user, active organization, role, and org memberships |
| `hotbed org ls\|switch\|create\|members\|invite` | List, switch, or create WorkOS organizations (one isolated Controller DO per org), and view or invite org members |
| `hotbed org join-token create\|ls\|rm` | Mint (`hbj_<org>.*`), list, or revoke org-scoped worker join tokens |
| `hotbed org api-token create\|ls\|rm` | Mint (`hbu_<org>.*`), list, or revoke org-scoped personal API tokens for `HOTBED_TOKEN` |
| `hotbed admin discover\|schema\|sql\|health\|gc` | Org-admin Explorer tools (strictly scoped to the caller's own organization): discover org Durable Objects (`discover`), inspect SQLite schemas (`schema [-b CONTROLLER\|COMMAND\|WORKER] [--name N \| --id HEX \| --command CMD]`), run read-only SQL (`sql QUERY [-p PARAM ...] [--command CMD]`), check controller health (`health`), and inspect tenant-scoped CladeFS R2 usage/GC (`gc [--action status\|dry_run_cell\|live_cell] [--cell CELL]`) |
| `hotbed api ENDPOINT [-X METHOD] [-f k=v] [-F k=v] [-d DATA] [--input FILE] [-H K:V] [-i] [--jq EXPR]` | Make an authenticated HTTP or ConnectRPC request to the controller (e.g. `hotbed api /healthz`, `hotbed api ClusterService/GetSnapshot --jq '.nodes \| length'`) |
| `hotbed daemon [run\|install\|uninstall\|status]` | Unified local Hotbed host service: keeps an outbound `/ws/questions` stream open to wake idle local T3 Code threads when questions are answered or dismissed, runs a Mac's worker and machined (which runs its workspaces' VMs), and shows a native macOS menubar status app (`install` sets up one LaunchAgent on macOS or systemd user service on Linux, migrating away from legacy separate services) |
