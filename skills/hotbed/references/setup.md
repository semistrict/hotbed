# Setting up a hotbed cluster

All machines are on one tailnet, owned by one Tailscale user. Nodes and clients are admitted by
that identity: there are no tokens.

```sh
# The binary goes in /usr/local/bin and state directories under /var/lib: on SELinux systems
# (Fedora, Asahi) systemd cannot run binaries or write logs under a home directory.

# The controller (any machine):
hotbed init /var/lib/hotbed               # data.address is what workers join
hotbed serve /var/lib/hotbed

# Each worker (Linux, as root; the directory on btrfs or zfs):
sudo hotbed join ubu1 /var/lib/hotbed --user you
sudo hotbed serve /var/lib/hotbed
#   --zfs-dataset pool/hotbed     where zfs volumes go (default <pool>/hotbed)
#   --unit-property DeviceAllow=/dev/kvm  grant what builds need, repeatable

# Repos, from anywhere on the tailnet:
export HOTBED_CONTROLLER=ubu1
hotbed repo add ratel https://github.com/you/ratel
hotbed step set ratel build --every-pull -- 'cargo build --all-targets && cargo nextest run --no-run'
hotbed repo ls
hotbed nodes
```

Steps are what brings a checkout up to date, in order. They work like database migrations: a
base runs a step once and again only when its script changes. `--every-pull` steps also run after
every pull; use that for builds and lockfile installs. Agents add their own steps with
`hotbed step add` when they find something missing (see the hotbed skill).

Nothing builds when a repo is added. A worker freshens a new base when a workspace is asked for
and the branch has moved or a step changed. The worker is one of those named with `--host`, or
any. It forks its newest base, pulls the branch's head with its submodules, runs the steps that
need to run, and snapshots the result as the next base. So a freshen is incremental. Only a
repo's first base builds from nothing, and that can take a while. `hotbed repo rebuild NAME` makes the next base build from nothing again, for when
incremental state has gone bad.

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
