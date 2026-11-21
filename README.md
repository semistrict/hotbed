# hotbed

Run AI coding agents' commands on a homelab fleet: old PCs plus Apple Silicon Macs on Asahi Linux.

- An agent leases a **workspace**: a btrfs snapshot of a repo with main already built (source, dependencies, toolchain, build output). It forks instantly, so a branch rebuilds only what changed.
- Each command runs in its own cgroup with CPU and memory limits. Spare CPU is shared.
- When a node runs low on memory, commands are frozen and swapped out instead of killed, then resumed later.
- A coordinator queues commands and places each workspace on the node with the warmest build.

## Use

```sh
hotbed init /var/lib/hotbed            # on the controller
hotbed run /var/lib/hotbed
hotbed join ubu1 /var/lib/hotbed       # on each Linux worker
hotbed run /var/lib/hotbed
HOTBED_CONTROLLER=ubu1 hotbed nodes    # anywhere on the tailnet
```

Every command prints one JSON envelope. Agents start with `hotbed skills get hotbed`.
