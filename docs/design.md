# hotbed design

hotbed runs agents' builds, tests and dev servers on a fleet of Linux machines (old PCs, Apple
Silicon laptops on Asahi) on one tailnet. Its one trick: every workspace is a copy-on-write fork of
a **warm base**, a checkout of main that is already built, so a branch compiles only what it changed.

Scope comes from two ends: the agent experience (the CLI, `hotbed skills get hotbed`) and the human
experience (the read-only cockpit in `ui/`, whose `src/api/types.ts` is the API it needs).

## Pieces

One binary. A node is the controller or a worker; any machine can be a client.

- **Controller** (`hotbed serve` on a controller state dir): the truth about what spans nodes, in
  SQLite (`hotbed.db`): repos and their steps, sessions, placement, schedules, history. Places
  workspaces on nodes, serves the API and the UI, and keeps a view of each worker folded from its
  events (below). It is not the truth about any worker's own state.
- **Worker** (`hotbed serve` on a worker state dir, as root): the truth about its own bases,
  workspaces and commands, in its SQLite (`worker.db`), so it picks everything up again after a
  restart. Admits commands, parks them under memory pressure, builds bases, and serves clients'
  calls about its workspaces directly.
- **Client**: agents and people. Every command prints one JSON envelope.

Auth is the tailnet: every request's caller is looked up with tailscaled's WhoIs and must be one of
the owner's machines (not tagged, not shared in from another tailnet). Controller and workers admit
the same callers; on the machine itself, only root and the configured local user.

The controller may be far from its workers (they go where hardware is cheapest), so nothing
between them assumes it is near or always there.

## Workers' state: an event stream

Every change to a worker's bases, workspaces and commands is written to `worker.db` together with
an **event** saying what changed, in one transaction, numbered by a sequence that only grows. An
event carries the whole entity as it now is (`Event` in `hotbed.proto`: `BaseChange` building, warm,
stale, failed, dropped, collected; `WorkspaceChange` created, ready, rebased, failed, synced,
active, idle, released, forgotten; `CommandChange` queued, waiting, admitted, parked, thawed,
exited, cancelled, forgotten, with the history entry when a command ends), plus a line for the
cockpit's feed when it is worth one. The records are the events folded.

- **Push.** The worker pushes its events in order (`PushEvents`), as soon as they are written,
  from after the controller's **applied** offset for it. The controller folds each into its view of
  the node (`node_state`), the session's timeline, history and the feed, in one transaction with the
  offset, so an event applies once however often it is sent.
- **Epochs and checkpoints.** A worker's database has an **epoch**, a random name. A controller
  that does not know the epoch (it is new, or the worker's database is), or whose next event is not
  the one after its applied (a gap, or a different event at the applied one: the push names its
  time), asks for a **checkpoint**: the worker's records and the last event they include, with the
  newest events the worker kept up to it. The controller applies those events first, for history
  and sessions' timelines, then makes the checkpoint its view. What the view still had and the
  checkpoint lacks is reported to its sessions as lost (commands cancelled, workspaces failed and
  released, scheduled runs cancelled). A worker whose database is an older copy (the controller
  has applied events it never wrote) starts a new epoch, and one that could not save a change (a
  full disk) sends its state as it holds it.
- **Retention.** A worker keeps events the controller applied for a day, for a controller restored
  from an older copy to catch up from, and never more than 50,000, whether or not the controller is
  in reach: a controller further behind gets a checkpoint. A queued command's wait is an event when
  its kind changes (memory, pressure, disk, its workspace preparing); its numbers go in heartbeats.
- **Heartbeat.** Live usage (CPU, memory, pressure, disk, each command's CPU and memory, a building
  base's progress) is lossy: a heartbeat every 5 s, the latest wins. It keeps the node online and
  names the sessions clients used on the worker since the last one delivered. Its reply is what the
  worker should host: the repos (with their steps and scratch epoch, so a step change or a rebuild
  needs no call of its own) and the workspaces of ended sessions to release. It is desired state,
  not a stream of orders: idempotent, and complete in each reply. A workspace request carries its
  repo too, which counts over the reply to any heartbeat sent before it.
- **Mixed versions.** Workers from before report their whole state (`Report`), which becomes
  their view; an upgraded worker delivers what its old outbox held once that way, then pushes
  events. The controller gives clients no worker address for a worker that does not push events
  (it admits only the controller), and a worker whose controller is older waits for it rather than
  stop. `Report` goes once every worker pushes.

## Talking to workers directly

The controller says where a workspace is (`Workspace.node_addr`, also on commands); the CLI then
syncs, runs commands, reads their output and stops them on the worker itself, so bundles and logs
never detour through the controller. `add`, `wait`, `ps` and the cockpit stay on the controller,
which sees every node. The controller still forwards workspace calls for older clients and the
cockpit.

- **Sessions.** A worker counts every call about a session's workspace or command as using the
  session and says so in its next heartbeat, so a session worked on only through its worker stays
  alive.
- **Without the controller.** Workers keep running, admitting, parking and building; their events
  wait. The CLI goes by where this machine last found the session's workspace, so `run`, `sync`,
  `logs` and `stop` keep working. What pauses: new workspaces (placement), `wait` and `ps`, and
  session expiry: the controller ends no session with a workspace on a worker it has not heard from,
  for up to a day. On a worker's own machine, the worker admits only root and the user its
  commands run as; anyone else gets a refusal saying so.

## Storage

`internal/storage` is the abstraction: named writable **volumes**, read-only **snapshots** of
them, and writable **clones** of snapshots, each O(1) and sharing blocks. Backends:

- **btrfs**: volumes and clones are subvolumes, snapshots are read-only subvolumes.
- **zfs**: volumes and clones are datasets, snapshots are `dataset@name`. Deleting a dataset whose
  snapshot still has clones first promotes one of them.

hotbed never changes a filesystem's configuration (no pools, quotas or mount options). The backend
is detected from the state directory's filesystem; a ZFS worker keeps its datasets under
`<pool>/hotbed`, mounted inside the state directory.

## Bases and workspaces

A repo (`hotbed repo add NAME URL`) has **setup steps**: idempotent bash scripts, in order, that
bring a checkout up to date the way a person would after pulling: install a toolchain, `pnpm
install`, `cargo build --all-targets`. They live on the controller, and agents edit them whenever
they find something missing (`hotbed step add`, which tries the script twice in the agent's own
workspace before saving it). Steps work like database migrations. Each base records the hash of
every step it ran or inherited. A generation runs a step only when its script changed since the
base it forks (or it is new), and `--every-pull` steps also run whenever the branch moved.

Bases are brought up to date only when a workspace asks for one; nothing builds in the background.
When a workspace is created, its node compares the head of the repo's branch and the repo's steps
with its newest warm base:

- If that base is at the head and has run every step as it is now, the workspace is a clone of
  it, at once.
- If a base for the same commit and steps (the same *recipe*) is already freshening (another
  workspace asked first), the workspace waits for it.
- Otherwise the node makes the next generation. It clones the newest warm base (or starts empty if
  there is none, and clones the repo blobless: the files of the commits it checks out, not every
  version of every file), fetches, checks out the branch's head and updates submodules recursively. It runs
  the steps that need to run, in one command like any other (so it gets limits, logs, history), and
  snapshots the result read-only. That snapshot is generation N, warm, and the waiting workspaces
  fork it.

A workspace is *preparing* while its base freshens; commands run in it wait for it, and
`hotbed add` waits too unless given `--no-wait`. A freshen that fails does not stop work. The
waiting workspaces fork the newest warm base instead, with a reason naming the step that failed.
The same recipe is not tried again until the commit or a step changes. A workspace *fails* only
when there is no warm base to fall back to. Because each generation
starts from the previous one, a freshen is a pull and an incremental build. Only
`hotbed repo rebuild NAME` makes the next generation start empty, for when incremental state has
gone bad. The controller places a workspace on a node that hosts the repo, preferring one with a
warm base, then the most free memory. An older base is deleted once no workspace uses it.

## What a command sees

Not a container: the host's kernel, users, network and `/usr`, with just enough of a private view
that a fork is indistinguishable from the base it came from.

- **A fixed path.** Each command has its own mount namespace with its workspace volume at `/work`
  and the checkout at `/work/<repo>`, the path the base was built at. Cargo itself copes with a
  moved workspace (`scripts/cargo-relocation-check.sh` on asahimbp, 2026-11-21: a fork at another
  path rebuilt 0 of 10 units, and 1 after an edit), but the toolchain and cache homes below are
  absolute paths, and build scripts and other tools bake paths in too.
- **Its tools and caches inside the workspace.** Everything a build would put in the home
  directory goes under `/work/.hotbed` instead, so the base carries it and every fork starts with
  it: `RUSTUP_HOME` and `CARGO_HOME` (toolchains from `rust-toolchain.toml`, the registry),
  `GOMODCACHE`, `GOCACHE` and Go toolchains (`GOTOOLCHAIN`), `XDG_CACHE_HOME`, `XDG_DATA_HOME`
  (pnpm's store), `COREPACK_HOME`, `npm_config_cache`. A fork never downloads what its base had.
- **The user's identity.** Commands run as the configured user in a login shell, so git and ssh
  configuration work as in a terminal.

## Getting code in

`hotbed sync WS` (and `hotbed exec` from inside a checkout) snapshots the agent's worktree, dirty
and untracked files included, as a commit (`git commit-tree` on a temporary index, so nothing in
the agent's repo changes), bundles it against the workspace's base commit, and the worker checks
it out. Only files that differ get new mtimes, so cargo rebuilds only what changed. (rsync would
carry the laptop's mtimes, which can be older than the base's build and make cargo skip a file.)

- When the snapshot's tree is the one last synced, nothing is sent.
- Ignored files (`target/`, `.env`) are never sent.
- Sync refuses a worktree with `skip-worktree` or `assume-unchanged` entries, whose real edits a
  commit would silently drop.
- One sync per workspace at a time; a sync waits for the one before it.

## Commands

Each command is a transient systemd unit (`systemd-run`) in `hotbed.slice`, so it survives a worker
restart: `CPUWeight=` for its share, `MemoryLow=` for its guarantee, `MemoryHigh=` as a soft
ceiling, no `MemoryMax=`. Output goes to a log file in the state directory. There is no time limit:
dev servers run until stopped or their workspace is released. An attached `hotbed exec` stops its
command if the client goes away; `--detach` leaves it running (`hotbed logs`, `hotbed stop`).
`hotbed exec` exits with the command's status; its own failures print an envelope and exit 1 or 2.

The worker admits a queued command when its guarantee fits in 86% of RAM beside the running ones'
and memory pressure (PSI some avg10) is under 22%, and the disk has room. When memory in use passes 93% it **parks** the
newest running command (`systemctl freeze`, then `memory.reclaim` pushes it to swap) and thaws the
oldest parked one when there is room again. Nothing is killed for memory.

## Scheduled commands

Recurring work (a nightly measurement, a soak) is a **scheduled command**: a repo, an optional
node, a five-field cron and a `hotbed run`'s command (a `RunCommandRequest`, so whatever `run`
learns, such as a measurement's isolation, a schedule carries). The controller keeps them and
their runs in SQLite (`schedules`, `schedule_runs`) and looks every 15 seconds for a slot that has
come.

- **A run is a session.** The controller starts a session called `NAME-YYYYMMDD-HHMM`, gives it a
  workspace of the repo (so a fork of the newest base, freshened if the branch moved), and runs the
  command in it detached. The cockpit and the CLI see it as any agent's session; `Session.schedule`
  says whose run it is.
- **Its end.** The worker's event of the command's end settles the run (the next tick does too, so
  nothing is lost across a restart). A run that succeeds ends its session. One that fails keeps it
  for the usual idle hour, from its end, for an agent to look inside. The reaper never ends a
  session whose run is in progress: no agent is there to touch it.
- **No overlap, no catching up.** A slot that comes while the previous run is in progress is
  recorded as skipped. A slot the controller first sees more than five minutes late (it was down)
  is recorded as missed; within five minutes (a restart, a deploy) it still runs.
- **A session that ends under its run** (`hotbed session end`, or a worker gone for good, whose
  command's end would never arrive) ends the run as cancelled, so the next slot can run.
- **Restarts.** A schedule's next slot is recorded before its run starts, so a controller that
  stops halfway loses that slot rather than running it twice. A run is recorded before its
  workspace and command; one left with no command is recorded as not started by the next tick.
- A schedule keeps its last 200 runs, however old.

## Prior art

crabbox (`~/src/crabbox`) leases remote boxes, rsyncs the worktree and runs commands over ssh.
hotbed differs where warm builds need it to: copy-on-write forks of a built base instead of
reused boxes, git-applied syncs that keep mtimes right, fixed paths, and commands as systemd units
with admission and parking.
