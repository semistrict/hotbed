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
hotbed repo add ratel https://github.com/you/ratel \
  --freshen 'cargo build --all-targets' --freshen 'cargo nextest run --no-run'
hotbed repo ls
hotbed nodes
```

`--freshen` commands are what a person runs after a pull to bring a checkout up to date. Nothing
builds when a repo is added. When a workspace is asked for and the branch has moved, the worker it
is placed on (one of those named with `--host`, or any) forks its newest base, pulls the branch's
head with its submodules, runs the freshen commands in order, and snapshots the result as the next
base. So a freshen is incremental; only a repo's first base builds from nothing, and can take a
while. `hotbed repo rebuild NAME` makes the next base build from nothing again, for when
incremental state has gone bad.

The controller serves a read-only cockpit at `http://CONTROLLER:7120/`.
