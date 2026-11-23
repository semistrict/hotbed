# Setting up a hotbed cluster

All machines are on one tailnet, owned by one Tailscale user. Nodes and clients are admitted by
that identity: there are no tokens.

```sh
# The binary goes in /usr/local/bin and state directories under /var/lib: on SELinux systems
# (Fedora, Asahi) systemd cannot run binaries or write logs under a home directory.

# The controller (any machine):
hotbed init /var/lib/hotbed               # data.address is what workers join
hotbed serve /var/lib/hotbed

# Each worker (single command, sets up in ~/.hotbed):
# - Linux: reuses btrfs/zfs if ~/.hotbed is on one, else creates a sparse file-backed ZFS pool
#   with autotrim=on, joins the cluster, and installs/starts hotbed-worker.service.
sudo hotbed worker init <controller>
# - macOS: boots an Ubuntu LTS QEMU (HVF) VM in ~/.hotbed/vm with sparse discard-enabled qcow2
#   ZFS + swap disks that grow on demand and punch holes back out when data is deleted.
hotbed worker init <controller>
#   A LaunchAgent runs `hotbed worker supervise`, which powers the VM off once it has had no
#   work for --sleep-after (default 30m; 0 never) and on again when a run is placed on it:
#   `hotbed nodes` shows it `asleep`, and `hotbed run` waits for it to boot (NODE_WAKING).
#   --no-autostart leaves it always on. `hotbed worker stop` stops the supervisor too, until
#   `hotbed worker start`.
#   hotbed worker status | start | stop | trim | ssh | destroy

# Or manually on Linux (as root; the directory on btrfs or zfs):
sudo hotbed join <controller> /var/lib/hotbed --user you
sudo hotbed serve /var/lib/hotbed
#   --zfs-dataset pool/hotbed     where zfs volumes go (default <pool>/hotbed)
#   --unit-property DeviceAllow=/dev/kvm  grant what builds need, repeatable

# Client commands call the controller worker init joined this machine to; no flag names it.
# On a machine that is no worker, set HOTBED_CONTROLLER=<controller> instead.
# For private GitHub repos and `gh` inside worker commands, log in ONCE on the controller
# (no per-node GitHub setup or SSH keys are needed on workers — git HTTPS, git SSH, and `gh`
# REST/GraphQL all delegate automatically to every worker, session, and command):
hotbed auth login
# Or headless/non-interactive via stdin:
# gh auth token | hotbed auth set github --token-file -
hotbed repo add ratel https://github.com/you/ratel     # makes project ratel holding it
hotbed repo add lnx https://github.com/you/lnx -p ratel  # a second repo of the same project
hotbed step set -p ratel build --every-pull -- '(cd lnx && cargo build) && (cd ratel && cargo build --all-targets)'
hotbed repo ls
hotbed nodes
```

A project's workspaces hold each of its repos at `/work/REPO`, side by side. Its steps are what
brings those checkouts up to date, in order, run in `/work`. They work like database migrations:
a base runs a step once and again only when its script changes. `--every-pull` steps also run
whenever any repo moved; use that for builds and lockfile installs. Agents add their own steps
with `hotbed step add` when they find something missing (see the hotbed skill).

Nothing builds when a repo is added. A worker freshens a new base of a project when a workspace
is asked for and a repo's branch has moved or a step changed. The worker is one of those named
with `hotbed project hosts PROJECT HOST...`, or any. It forks its newest base, pulls each repo's
branch head with its submodules, runs the steps that need to run, and snapshots the result as
the next base. So a freshen is incremental. Only a project's first base builds from nothing,
and that can take a while. `hotbed project rebuild PROJECT` makes the next base build from
nothing again, for when incremental state has gone bad.

## Measurements

A worker takes measurements (`hotbed run --bench`) once its config, `hotbed.json` in its state
directory, names its measurement CPUs under `worker.bench`; it reads it when it starts:

```json
"bench": {"cpus": "4-7,12-15", "l3": "fff0", "workL3": "000f", "workMB": 40, "capClocks": true, "disk": "/mnt/scratch"}
```

- `cpus`: a cpulist the measurement has to itself, a cpuset partition; at least one CPU must be
  left for everything else.
- `l3` and `workL3` (both or neither): the measurement's and everyone else's L3 ways, capacity
  bitmasks for resctrl (Intel CAT, AMD L3 CAT). `workMB`: everyone else's memory bandwidth, in
  resctrl MBA's units. Leave them out where there is no resctrl (Apple Silicon).
- `capClocks`: cap the other CPUs at their nominal clock (ACPI CPPC). Refused where a CPU shares
  its clock with a measurement CPU (a cluster, on Apple Silicon).
- `disk`: a path on the disk measurements keep their stores on; other commands' IO on it is
  capped while one runs.

The controller serves a read-only cockpit at `http://CONTROLLER:7120/`.
