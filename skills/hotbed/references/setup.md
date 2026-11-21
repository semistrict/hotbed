# Setting up a hotbed cluster

All machines are on one tailnet, owned by one Tailscale user. Nodes and clients are admitted by
that identity: there are no tokens.

```sh
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
hotbed repo add ratel https://github.com/you/ratel \
  --freshen 'cargo build --all-targets' --freshen 'cargo nextest run --no-run'
hotbed repo ls
hotbed nodes
```

`--freshen` commands are what a person runs after a pull to bring a checkout up to date. Every
worker that hosts the repo (all, or those named with `--host`) checks out the default branch with
its submodules, runs them in order, and snapshots the result as a warm base. When the branch moves,
the next base is built from the previous one, so it is incremental too. A repo's first base builds
from nothing and can take a while; `hotbed add` answers `NO_WARM_BASE` with its progress until it
is ready.

The controller serves a read-only cockpit at `http://CONTROLLER:7120/`.
