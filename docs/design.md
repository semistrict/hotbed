# hotbed design

hotbed runs agents' builds, tests and dev servers on a fleet of Linux machines (old PCs, Apple
Silicon laptops on Asahi) on one tailnet. Its one trick: every workspace is a copy-on-write fork of
a **warm base**, a checkout of main that is already built, so a branch compiles only what it changed.

Scope comes from two ends: the agent experience (the CLI, `hotbed skills get hotbed`) and the human
experience (the cockpit in `ui/`, whose `src/api/types.ts` is the API it needs). The cockpit
watches the cluster and is where the user answers agents' questions; whatever it does, the CLI
does too.

## Pieces

One binary. A node is the controller or a worker; any machine can be a client.

- **Controller** (`hotbed serve` on a controller state dir): the truth about what spans nodes, in
  SQLite (`hotbed.db`): repos, projects and their steps, sessions, placement, schedules, history. Places
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
  worker should host: the projects whose bases it keeps (with their repos, steps and scratch epoch,
  so a step change or a rebuild needs no call of its own), the workspaces of ended sessions to
  release, and the build it should run. It is desired state,
  not a stream of orders: idempotent, and complete in each reply. A workspace request carries its
  project too, which counts over the reply to any heartbeat sent before it.
- **No legacy reporting.** The pre-event-stream `Report` RPC and worker outbox are retired: every
  worker syncs state exclusively through `PushEvents` and `Heartbeat`, and admits clients directly
  at its tailnet address.

## Talking to workers directly

The controller says where a workspace is (`Workspace.node_addr`, also on commands); the CLI then
calls the worker directly for data-plane operations (`Sync`, `WatchWorkspace`, `RunCommand`,
`StopCommand`, `FollowLog`), so bundles, commands and live logs never detour through the
controller. Controller data-plane forwarding for older clients is retired (only the controller's
own scheduler calls `RunCommand` and `StopCommand` on workers). Workspace placement and lifecycle
(`CreateWorkspace`, `ReleaseWorkspace`), cluster-wide views (`ListWorkspaces`, `ListCommands`/`ps`,
`Poll`), and cockpit read/diagnostic/shell calls (`GetWorkspace`, `GetCommand`, `ListFiles`,
`GetFile`, `OpenShell`, `GetLogLines`, `InspectCommand`, `TraceCommand`, `SampleCommand`,
`GetProfile`, `GetDaemonLogs`) stay on the controller, which sees every node.

- **Sessions.** A worker counts every call about a session's workspace or command as using the
  session and says so in its next heartbeat, so a session worked on only through its worker stays
  alive.
- **Without the controller.** Workers keep running, admitting, parking and building; their events
  wait. The CLI goes by where this machine last found the session's workspace, so `run`, `sync`,
  `logs` and `stop` keep working. What pauses: new workspaces (placement), `poll` and `ps`, and
  session expiry: the controller ends no session with a workspace on a worker it has not heard from,
  for up to a day. On a worker's own machine, the worker admits only root and the user its
  commands run as; anyone else gets a refusal saying so.

## Projects

A **project** is what sessions work on and questions are about, as in T3 Code: one or more repos,
each named by its canonical git remote (`github.com/semistrict/hotbed`, whatever form the remote
was written in). Projects are their own table (`projects`, with `project_repos` naming the repos
each holds), and a repo may be in several. A project is also what a workspace is of: its repos
checked out side by side, built by its setup steps (below), so a session works on all of them at
once.

- **Every session belongs to one.** `session start -p NAME` names it. Otherwise the CLI sends its
  checkout's remote and the session joins the one project that holds it; a repo no project holds
  becomes a project of its own name, made then; a repo several projects hold, or no checkout at
  all, needs `-p`. A scheduled run joins its repo's project the same way.
- **One project per repo unless shared.** `project add` refuses a repo another project holds
  unless given `--shared`, because a repo in two projects makes every session starting in its
  checkouts name its project. `project rm-repo` takes a repo out. Every change (created, repo
  added, repo removed) is kept in `project_changes` with who made it: a session, a person, or
  the controller, which says why it made a project by itself.
- **The cockpit is organized around them.** Its home lists projects with their sessions, running
  commands and questions; choosing one scopes every page (sessions, questions, commands,
  history, workspaces, schedules) to it. The machines (Cluster, Nodes) are every project's.

## Session events: poll

Whatever happens later to what a session started reaches its agent the same way: as an event of
the session, which `Poll` returns. An event is one of a command's (admitted, parked, thawed,
paused, resumed, isolation lost, exited, cancelled), a workspace's (ready, failed), a question's
(answered or dismissed, with the question), or a review's (finished, failed, responded): a
`oneof` of messages, each with its own `What` enum, never a word to parse. Events are rows of
`session_events`, numbered by the controller; workers report command and workspace changes in
their event stream, and the question and review stores add theirs in the same transaction as the
change.

- **Long-poll.** `Poll` returns at once with the events after a cursor, or waits up to two minutes
  for some. It changes nothing, so a client makes it again when a restart cuts it off. The client
  keeps the cursor, a file per session on its machine that the CLI and its MCP server share,
  saved once a reply is in hand: a lost reply loses no events. `WatchSession` streams the same
  events, for clients that hold a connection (the MCP server's keep-alive).
- **One way to wait.** There is no per-kind wait (no `question wait`, `review wait`): an agent
  detaches long work and polls. Attached `run` is for short work.

## Questions

Agents ask the user questions the way AskUserQuestion asks them (the same JSON: a question, a
header, two to four options with descriptions and previews, multiSelect), without stopping for
the answer. Each is an entry on its project's list, which every session of the project shares
and may edit or delete.

- **Lifecycle.** *open* (the user's inbox, in an order the user sets) → *answered* (picks, words
  of the user's own, a note) or *dismissed* (no answer will come), which wait in the **outbox**
  until an agent acknowledges them → *handled*. Any session may delete a question (*deleted*).
- **Nothing is removed.** `questions` holds each question as it is; `question_changes` holds every
  change (asked, edited, moved, answered, dismissed, handled, deleted) with when, which session
  (empty for the user) on which machine, why, and the question as the change left it.
- **Where it was asked.** A question keeps its asker's directory, origin, branch, commit and
  changed files, and the project's repo it is about (the asker's checkout, when the project
  holds it).
- **Hearing back.** An answer or dismissal is an event of the asking session, carrying the
  question as answered: its agent's `hotbed poll` returns it.

## Reviews

A review is of an exact version: a repo's base and head commits (no base: the whole tree), in the
requesting session's project. A dirty worktree is reviewed as a snapshot commit on HEAD (the one
`sync` makes), and every head is pinned under `refs/hotbed/reviews/` in the requester's checkout,
so the reviewer can read it and git keeps it.

- **Lifecycle.** *requested* → *in progress* (a reviewer session claims it; only it reports
  findings and ends it) → *done*, with a summary, or *failed*, with why; *cancelled* by its
  project's sessions before it ends.
- **Findings** take ReportFindings' JSON (summary, failure scenario, category, severity, short
  summary, verdict, file and line) and any number of locations besides: path, lines and the
  excerpt the reviewer saw, so the cockpit shows them without the code. The requester decides each:
  accepted, dismissed, or open again.
- **Responses.** The user responds in the cockpit, per finding or to the review: a quick response
  (verify, fix if verified, won't fix), a comment, or both. Agents reply the same way. The
  requester reads them after a cursor; a response, and the review's end, are also events of its
  session, which its `poll` returns.
- **Nothing is removed.** `reviews` and `review_findings` hold each as it is; `review_events`
  holds every change (with the review or finding as it left it) and every response, with when,
  which session (empty for the user) and which machine.

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

A repo (`hotbed repo add NAME URL`) is where code comes from: a URL and the branch bases follow.
Each project's bases and workspaces check out every repo it holds at `/work/<repo>`, side by
side. A project has **setup steps**: idempotent bash scripts, in order, run in `/work`, that
bring the checkouts up to date the way a person would after pulling: install a toolchain, `pnpm
install`, `cargo build --all-targets`. They live on the controller (the project's `setup`, with
the hosts that keep its bases and its scratch epoch), and agents edit them whenever they find
something missing (`hotbed step add`, which tries the script twice in `/work` of the agent's own
workspace before saving it). Steps work like database migrations. Each base records the hash of
every step it ran or inherited. A generation runs a step only when its script changed since the
base it forks (or it is new), and `--every-pull` steps also run whenever any repo moved.

A project's bases are kept only by its hosts (`hotbed project hosts`; none means every worker),
and only while every remote it holds is a repo of the cluster: a workspace of it could not be
made otherwise, so its bases go once unused.

Bases are brought up to date only when a workspace asks for one; nothing builds in the background.
When a workspace is created, its node compares each repo's branch head and the project's steps
with its newest warm base:

- If that base has every repo at its head and has run every step as it is now, the workspace is
  a clone of it, at once.
- If a base for the same commits and steps (the same *recipe*) is already freshening (another
  workspace asked first), the workspace waits for it.
- Otherwise the node makes the next generation. It clones the newest warm base (or clones the
  worker's shared base rootfs snapshot when there is none, and clones each repo blobless: the files
  of the commits it checks out, not every version of every file), fetches each repo, checks out its
  branch's head and updates submodules recursively, and removes the checkouts of repos the project
  no longer holds. It runs the steps that need to run, in one command like any other (so it gets
  limits, logs, history), and snapshots the result read-only. That snapshot is generation N, warm,
  and the waiting workspaces fork it.

A workspace is *preparing* while its base freshens; commands run in it wait for it, and so does
`hotbed run` (a detached run that syncs nothing is queued in it instead). A freshen that fails does not stop work. The
waiting workspaces fork the newest warm base instead, with a reason naming the step that failed.
The same recipe is not tried again until a commit or a step changes. A workspace *fails* only
when there is no warm base to fall back to. Because each generation
starts from the previous one, a freshen is a pull and an incremental build. Only
`hotbed project rebuild NAME` makes the next generation start empty, for when incremental state has
gone bad. An older base is deleted once no workspace uses it.

Bases and workspaces were once of one repo each, with steps on the repo. Every project then held
one repo, named after it, and a base of one repo was laid out as its project's would be, so the
databases converted in place (worker migration 2, controller migrations 10 and 11): each repo's
steps moved to its project with `cd <repo>` in front, and nothing was rebuilt.

## Placement

The controller places every run, not every session: `hotbed run` asks it (`CreateWorkspace` with a
`Placement`) where to go, and gets the session's workspace on that node, or a new one there. A
session has at most one workspace per node, of its project, made the first time it runs there,
all released when it ends. A run names the project's repo it syncs and starts in; one of a repo
the project does not hold is refused. The agent's checkout is the truth: each run sends it to the workspace
it runs in, and whatever earlier commands wrote stays on their node.

`internal/controller/placement.go` decides. It leaves out nodes the run cannot use: offline, on another build than
the controller's (it follows within minutes; until then a run waits for it, retryably), not
keeping the project's bases, not `--node`, not `--arch`, without measurement CPUs for `--bench`, or
with less RAM than `--mem-low`. Each node left weighs its load (CPU busy, plus queued commands
per CPU, plus 1 when `--mem-low` does not fit now), plus 0.3 when the session has no workspace
there (a fresh fork rebuilds what the session had compiled), plus 0.5 more when the node
has no warm base (its first build is from nothing), plus 10 when its newest base failed with none
warm; the least wins. So a session stays put until its node is clearly busier, and a saturated
warm node loses to an idle cold one, which then gets a base of its own: no node starves for want
of a first build.

A run that syncs no checkout changes only what its workspace holds, so it stays in the session's
workspace while one fits its restrictions, and fails retryably (`WORKER_UNREACHABLE`) when that
node is offline rather than start over; restrictions that rule out every node with one start it
afresh where they allow. Placements are serialized, so two runs at once never both make a session
a workspace on one node. A failed workspace is released when the next run is placed. A node that
cannot make a workspace when asked (out of the controller's reach, or unable to read the repo's
remote: a machine without a repo's credentials) is passed over for the next node placement picks;
with none left, the run fails as the last one did.

A worker in a VM sleeps when idle. Its host's supervisor (`hotbed worker supervise`, run by a
LaunchAgent on macOS) holds the controller's `Standby` stream open, saying whether the VM runs,
and starts or stops it as the controller says (`internal/controller/standby.go`). The controller
puts the node to sleep once it has had no work (a session's command, a workspace being made,
clients using it directly) for its sleep-after, and nothing is under way that powering off would
break (a base freshened on its own); it never decides so while a run is being placed. A node
asleep, or booting, is `asleep`, not offline: placement weighs it like an idle node plus 1, and
one it picks is woken and waited for, then the run placed again; past 40 seconds the client
gets `NODE_WAKING` and calls again. Its sessions are reaped as if it were up. Free page
reporting cannot give an idle VM's memory back on macOS (`dead-ends/`), so powering off does.

## What a command sees

A chrooted rootfs (`systemd` `RootDirectory=`, `MountAPIVFS=yes`) cloned from one shared worker
base rootfs image, on the host's kernel and network—with no dependence on host-installed toolchains
or tracing packages.

- **Chroot with passwordless `sudo` for the build user.** Every command runs in a transient
  systemd service unit (`RootDirectory=`, `MountAPIVFS=yes`, `AmbientCapabilities=CAP_PERFMON`,
  `/sys/kernel/tracing` and `/sys/fs/cgroup` bound) as the worker's configured build user, with no
  user namespaces or idmapped mounts. The base image configures passwordless `sudo`
  (`/etc/sudoers.d/hotbed-user`) for the build user so setup steps, commands, and interactive shells
  can run `sudo` as real root inside the chroot (e.g. `sudo apt-get install -y PKG`, adjusting
  cgroup limits, etc.). Interactive shells (`hotbed shell`) and worker-invoked checkout operations
  (`git`, `hotbed __sync-apply`) run chrooted inside a transient systemd scope (`unshare --mount
  --propagation slave` + `chroot`). The chroot is a build-environment isolation boundary for trusted
  code on a single-owner tailnet, not an adversarial sandbox against hostile code or tailnet access.

- **Shared base rootfs image (`internal/baseimage`).** Each worker pulls a minimal Debian OCI
  rootfs (`public.ecr.aws/debian/debian:trixie-slim`) for its architecture, provisions it once with
  core build utilities (`git`, `gh`, `curl`, `build-essential`, `pkg-config`, `python3`), latest
  stable Rust (`rustup`/`cargo` in `/work/.hotbed`), and all five Linux tracing tools (`strace`,
  wrapper-free `linux-perf`, `bpftrace`, `elfutils`/`eu-stack`, `gdb`), and caches it as a
  read-only CoW snapshot (`baseimg-<hash>`). Cold project bases and `hotbed project rebuild` clone
  from this snapshot; project-specific toolchains belong to each project's setup steps.
- **A fixed path.** Inside its root, every project's repos sit at `/work/<repo>`
  and commands start in their repo's checkout (or `/work`). Each command gets a private `/tmp` and
  `/var/tmp` (`<volume>/.tmp/<command>/tmp`) and a private `0700` `XDG_RUNTIME_DIR` at
  `/run/user/<uid>` (`<volume>/.tmp/<command>/run`), removed when it ends and before a base is
  snapshotted, with `TMPDIR=/tmp`, `/etc/resolv.conf` bound read-only for DNS, and a read-only
  `/etc/containers/containers.conf.d/50-hotbed.conf` setting `cgroup_manager = "cgroupfs"` and
  `events_logger = "file"` so rootless `podman` and `buildah` run cleanly without a systemd user
  session while keeping containers inside the command's own cgroup.
- **Its tools and caches inside the workspace.** Everything a build would put in the home
  directory goes under `/work/.hotbed` instead, so the base carries it and every fork starts with
  it: `RUSTUP_HOME` and `CARGO_HOME` (toolchains from `rust-toolchain.toml`, the registry),
  `GOMODCACHE`, `GOCACHE` and Go toolchains (`GOTOOLCHAIN`), `XDG_CACHE_HOME`, `XDG_DATA_HOME`
  (pnpm's store), `COREPACK_HOME`, `npm_config_cache`, and `HOTBED_BIN` (`/work/.hotbed/bin`).
- **Deterministic environment and ephemeral freshen auth.** Commands run with a fixed `PATH`
  (no host login shell or host `PathSuffix`). Only `freshen` units receive ephemeral read-only bind
  mounts of the worker user's host git/gh/ssh credentials so private repos clone without baking
  host credentials into the base snapshot.
- **In-rootfs and in-VM active tracing (`internal/inspect`).** For chroot workspaces, the worker
  resolves and runs `strace`, `perf`, `bpftrace`, `eu-stack`, and `gdb` as the host's root inside
  the shared base image's rootfs via a private mount namespace with host `/proc`, `/sys`, and `/dev`
  rbind-mounted, so zero host tracer packages are required and DWARF/library paths match the traced
  processes directly. For Firecracker VM workspaces, `hotbed-guestd` runs `internal/inspect`
  directly inside the guest against `/sys/fs/cgroup/hotbed/<id>` and streams results back over
  vsock. In both modes it finds a command's processes in every cgroup below its unit's.

## Getting code in

`hotbed run` from inside a checkout (and `hotbed sync`) snapshots the agent's worktree, dirty
and untracked files included, into the workspace's checkout of that repo, as a commit (`git commit-tree` on a temporary index, so nothing in
the agent's repo changes), bundles it against the workspace's base commit, and the worker checks
it out. Only files that differ get new mtimes, so cargo rebuilds only what changed. (rsync would
carry the laptop's mtimes, which can be older than the base's build and make cargo skip a file.)

- Submodules go the same way, recursively: each populated one is snapshotted first (dirty,
  untracked and unpushed work included) and the parent's snapshot records that commit, so the
  top tree says the whole state. Each submodule whose commit the workspace lacks gets its own
  bundle, against the commit the base checked out there; one only on its origin, when this
  checkout lacks the base's commit, is left to `git submodule update`, and one new since the
  base goes whole. Submodules are matched to the base by name, so a moved one keeps its base
  commit and its repo in the workspace. The worker checks the bundles out parents first, makes a
  repo for a new submodule as `git submodule update` would, deinits submodules the snapshot
  dropped, and updates the rest from their origins.
- Sync refuses a repo nested in the worktree that no `.gitmodules` entry names: no workspace
  could check it out.
- When the snapshot's tree is the one last synced, nothing is sent. Snapshot commits take
  HEAD's date, so the same state makes the same commit and tree.
- Ignored files (`target/`, `.env`) are never sent.
- Sync refuses a worktree with `skip-worktree` or `assume-unchanged` entries, whose real edits a
  commit would silently drop.
- One sync per workspace at a time; a sync waits for the one before it.

## Commands

Each command is a transient systemd unit (`systemd-run`) in `hotbed.slice`, so it survives a worker
restart: `CPUWeight=` for its share, `MemoryLow=` for its guarantee, `MemoryHigh=` as a soft
ceiling, no `MemoryMax=`. The work slice runs at idle CPU and minimum IO weight (`CPUWeight=idle`,
`IOWeight=1`), and non-measurement commands default to `Nice=19` and `IOSchedulingClass=idle`, so
normal work yields to desktop and system processes while still sharing spare CPU among commands by
their `CPUWeight=`. Output goes to a log file in the state directory. There is no time limit:
dev servers run until stopped or their workspace is released. An attached `hotbed exec` stops its
command if the client goes away; `--detach` leaves it running (`hotbed logs`, `hotbed stop`).
`hotbed exec` exits with the command's status; its own failures print an envelope and exit 1 or 2.

The worker admits a queued command when its reservation fits in 86% of RAM beside those of the
running and parked commands, base builds among them, and memory pressure (PSI some avg10) is under
22%, and the disk has room. A command's reservation is its guarantee or, if more, the most memory
it has held of its own (anonymous and swapped, not file cache), as a build needs again what its
last link did. A base build is guaranteed what the project's last base build on the node held, so
work admitted beside it leaves it that room from its first step. When memory in use passes 93%, or
memory pressure reaches 40% (the kernel counts mapped file pages a thrashing build faults straight
back in as available, so memory in use can look fine while the node stalls), it **parks** the
newest running command (`systemctl freeze`, then `memory.reclaim` pushes it to swap), one each 20
seconds for pressure, and thaws the oldest parked one when there is room again and pressure is
under 22%. Nothing is killed for memory by the worker. Beneath all that, the work slice itself is
bounded (`MemoryHigh=`, `MemoryMax=` at its start), leaving the host an eighth of its RAM (at least
1 GiB): past High the kernel throttles and reclaims the commands together, past Max it kills one of
them inside the slice, so an overload waits or loses a command but never takes the host down. The
worker measures its memory within that bound. The measurement slice is outside it.

## Measurements

`hotbed run --bench` makes a command a **measurement**: numbers taken from it are comparable with
earlier ones from the same node, whatever else the node runs. A worker takes measurements when
its config names its measurement CPUs (`bench` in `hotbed.json`, written by the host's setup
step; `hotbed nodes` shows them as `benchCpus`). `internal/bench` does the isolating, after
fxjob-isolate, which did it for fxjob before hotbed replaced it.

- **One at a time.** A measurement waits for the one before it, and for running work to fit in
  the CPUs it leaves them (the commands' CPU use, measured each second). While it waits for room,
  work queued after it waits behind it, so the work ahead drains; while it waits for another
  measurement, work goes on past it.
- **Isolated for its whole run.** Just before it starts, its slice (beside the commands' slice,
  in the same parent, so it shares their memory but not their IO caps) becomes a cpuset partition
  root of the measurement CPUs, handed them through every ancestor's `cpuset.cpus.exclusive`: the
  kernel takes them from every other cgroup. Unbound workqueues and movable IRQs go to the other
  CPUs. Where configured, resctrl gives it most of the L3 and the others little memory
  bandwidth, and the other cores' clocks are capped at nominal, leaving it the power budget.
- **Quiet disks.** Other commands may write at most a bound (`workWriteMB` in the bench config,
  8 MB/s by default) to each disk or pool it uses. On a block device (the scratch disk, a btrfs
  store's disk) the work slice's `io.max` caps them. A ZFS pool is written by ZFS's own threads,
  which no cgroup cap reaches, so the worker reads each dataset's writes (its objset kstat) every
  step and holds (parks) the commands of volumes writing past half the bound, just before the
  measurement starts and while it runs. Queued commands of other volumes wait for it to end. The
  pool is synced before it starts. Held commands thaw once it ends.
- **Nothing in its way.** It never swaps (`MemorySwapMax=0`), is never parked, and runs in its own
  sibling slice at normal CPU and IO priority (nice 0, default IO class).
- **Checked.** Its isolation is checked every second and as it ends: a measurement whose
  partition the kernel invalidated or cut short, or whose workqueues or cache class went, fails.
  So does one whose disks saw others write past the bound anyway, averaged over 30 seconds (or
  its run, if longer), as a caller's own contamination check averages over its rounds. A writer
  is held within a sample or two of starting, so its first burst alone doesn't fail it. The error
  names the disk or pool, the bytes, and the heaviest volume.
- **Given back.** What the isolation changes is saved first under `/run/hotbed/bench` (as long as
  the kernel's settings last), and restored once no measurement runs: after it ends, or when a
  restarted worker finds none running. Restored clocks are read back; a failed restore is tried
  again later.

## Scheduled commands

Recurring work (a nightly measurement, a soak) is a **scheduled command**: a repo, an optional
node, a five-field cron and a `hotbed run`'s command (a `RunCommandRequest`, so whatever `run`
learns, such as a measurement's isolation, a schedule carries). The controller keeps them and
their runs in SQLite (`schedules`, `schedule_runs`) and looks every 15 seconds for a slot that has
come.

- **A run is a session.** The controller starts a session called `NAME-YYYYMMDD-HHMM`, gives it a
  workspace of its project (a fork of the newest base, freshened if a repo moved), and runs the
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

## Updates

Nothing is deployed by hand. Every push to main is tested and built by CI
(`.github/workflows/build.yml`) for each platform hotbed runs on (linux/amd64, linux/arm64,
darwin/arm64), stamped with its build number (`internal/version`), and published as the GitHub
release `build-N` with the binaries' `SHA256SUMS`.

- **The controller follows CI.** With `updates` in its config (the repo, and a file holding a
  GitHub token that may read its releases), it looks for a newer release every two minutes. It
  fetches every platform's binary into `builds/N` in its state directory, each checked against
  the sums, installs its own over its binary in one rename, and starts it in its own place
  (exec, with the same arguments): what runs it need not restart it. A dev build (built by hand, build 0) is replaced the same way: branches are
  tried in hotbed sessions, not on the fleet's own nodes.
- **Everything else follows the controller, from the controller.** Workers never reach GitHub.
  The controller serves the build it runs at `/builds/N/hotbed-OS-ARCH`, behind the same gate as
  its API, and hands each worker its platform's build (number, path, sha256) in every heartbeat's
  reply. A worker on another build, newer or older, downloads it, checks its sum, installs it
  over itself and execs it; its commands, systemd units, keep running across the restart, as
  they do across any. A client on a CI build sees the controller's build on every answer
  (`Hotbed-Build`) and installs it after the command it ran; `hotbed upgrade` does it on demand.
  So every node runs the controller's build.
- **What it keeps.** The controller keeps the build it runs and fetches the next; CI keeps the
  newest 20 releases.
- A build that fails to start is not rolled back: CI's tests are what stand before the fleet.
  The fleet skill's `hotbed/deploy.sh` installs a chosen release by hand.

## Prior art

crabbox (`~/src/crabbox`) leases remote boxes, rsyncs the worktree and runs commands over ssh.
hotbed differs where warm builds need it to: copy-on-write forks of a built base instead of
reused boxes, git-applied syncs that keep mtimes right, fixed paths, and commands as systemd units
with admission and parking.
