# Setting up a hotbed cluster

The controller is a Cloudflare Worker (`cf-controller/`, deployed by CI) at an HTTPS URL; it also
serves the cockpit. Workers listen on no network address: each connects out to the controller
over an authenticated WebSocket tunnel, so a machine needs only outbound HTTPS. Auth is WorkOS
sign-in for people and org-scoped tokens for machines (`hbj_` join, `hbw_` worker, `hbu_` personal
API); there is no tailnet and no controller to install on a machine.

```sh
# Sign in once per client machine (uses a one-time device code, so it works identically locally
# and over SSH without a localhost callback). Stores the controller URL for later commands:
hotbed login https://CONTROLLER

# A join token (hbj_...) comes from the cockpit's Organization page ("Mint join token").
# Each worker (single command, sets up in ~/.hotbed; token via --token or HOTBED_TOKEN, or
# taken from your hotbed login when you are signed in):
# - Linux: joins the cluster in ~/.hotbed/worker (any filesystem; workspaces run in Firecracker
#   microVMs on cladefs) and installs/starts hotbed-worker.service.
sudo hotbed worker init https://CONTROLLER --token hbj_...
# - macOS (Apple silicon): the worker runs natively, as you, in ~/.hotbed/worker, every
#   workspace a VM of machined's (cladefs's VMM on Hypervisor.framework), one per workspace,
#   booted from a cell of the cladefs daemon machined embeds; free page reporting gives an
#   idle VM's memory back to macOS. hotbed's daemon runs machined and the worker (macworker),
#   restarting them if they end: `hotbed worker init` installs it (`hotbed daemon install`,
#   `hotbed daemon status`).
hotbed worker init https://CONTROLLER --token hbj_...
#   hotbed worker status | start | stop | destroy

# Or manually on Linux (as root):
sudo hotbed join https://CONTROLLER /var/lib/hotbed --user you --token hbj_...
sudo hotbed serve /var/lib/hotbed       # runs the worker; it exchanges the join token for its own hbw_ token
#   --unit-property DeviceAllow=/dev/kvm  grant what builds need, repeatable

# Client commands call the controller you signed in to (or HOTBED_CONTROLLER=https://CONTROLLER;
# HOTBED_TOKEN=hbu_... for a machine with no browser login).
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

The cockpit is at the controller's HTTPS URL (sign in with WorkOS).
