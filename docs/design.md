# hotbed design

hotbed runs agents' builds, tests and dev servers on a fleet of Linux machines (old PCs, Apple
Silicon laptops on Asahi) on one tailnet. Its one trick: every workspace is a copy-on-write fork of
a **warm base**, a checkout of main that is already built, so a branch compiles only what it changed.

Scope comes from two ends: the agent experience (the CLI, `hotbed skills get hotbed`) and the human
experience (the read-only cockpit in `ui/`, whose `src/api/types.ts` is the API it needs).

## Pieces

One binary. A node is the controller or a worker; any machine can be a client.

- **Controller** (`hotbed run` on a controller state dir): the cluster's source of truth in SQLite
  (`hotbed.db`): nodes, repos, bases, workspaces, commands, history, samples. Places workspaces on
  nodes, serves the API and the UI, and proxies workspace traffic to the worker that holds it.
- **Worker** (`hotbed run` on a worker state dir, as root): owns its volumes and commands, keeps its
  own SQLite (`worker.db`) so it picks everything up again after a restart, admits commands, parks
  them under memory pressure, builds bases, and reports to the controller.
- **Client**: agents and people. Every command prints one JSON envelope.

Auth is the tailnet: every request's caller is looked up with tailscaled's WhoIs and must be owned
by the cluster's owner. Workers also accept the controller's node.

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

A repo is registered once (`hotbed repo add NAME URL --freshen CMD...`) with the commands that
bring a checkout up to date, the way a person would after pulling: `pnpm install`,
`cargo build --all-targets`, `cargo nextest run --no-run`. Each worker that hosts the repo keeps a
base per generation of its default branch:

1. Clone the newest base (or start empty), fetch, check out the branch's new commit and update
   submodules recursively.
2. Run the freshen commands in it, as commands like any other (so they get limits, logs, history).
3. Snapshot it read-only: that snapshot is generation N, warm.

A workspace is a clone of the newest warm base on the node it is placed on. The controller places
it on a node that hosts the repo, preferring the most free memory. An older base is deleted once
no workspace uses it.

## What a command sees

Not a container: the host's kernel, users, network and `/usr`, with just enough of a private view
that a fork is indistinguishable from the base it came from.

- **A fixed path.** Each command has its own mount namespace with its workspace volume at `/work`
  and the checkout at `/work/<repo>`, the path the base was built at. Cargo bakes absolute paths
  into its fingerprints; `scripts/cargo-relocation-check.sh` measures what moving costs.
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

## Prior art

crabbox (`~/src/crabbox`) leases remote boxes, rsyncs the worktree and runs commands over ssh.
hotbed differs where warm builds need it to: copy-on-write forks of a built base instead of
reused boxes, git-applied syncs that keep mtimes right, fixed paths, and commands as systemd units
with admission and parking.
