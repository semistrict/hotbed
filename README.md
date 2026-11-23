# hotbed

> [!CAUTION]
> ## WARNING: ALPHA SOFTWARE — REGULARLY CRASHES MY ENTIRE OS — TOTALLY UNSECURED
>
> 1. **This is ALPHA software that regularly crashes my entire OS.** Hotbed aggressively drives cgroups, memory pressure, swap/zram, btrfs/ZFS snapshots, eBPF profiling, and QEMU VMs. Expect kernel hangs, OOM lockups, and data loss on machines you run it on.
> 2. **This software is TOTALLY UNSECURED — ONLY run it on a private Tailscale tailnet you exclusively control.**
>    - There is **no security boundary** between workspaces and the host: commands run in a plain chroot (`systemd` `RootDirectory=`) as the build user with **passwordless `sudo` to real root**.
>    - Cluster RPCs, the web cockpit, and worker APIs speak **unencrypted HTTP** and rely solely on Tailscale peer identity.
>    - **Never** expose hotbed to the public internet, shared networks, multi-user machines, or untrusted code.

Run AI coding agents' commands on a homelab fleet: Linux machines plus Apple Silicon Macs (either on Asahi Linux or via a managed Ubuntu QEMU VM on macOS).

- An agent leases a **workspace**: a copy-on-write snapshot (btrfs or ZFS) of a project's repos with main already built (source, dependencies, toolchain, build output). It forks instantly, so a branch rebuilds only what changed.
- Each command runs in its own systemd unit and cgroup with CPU and memory limits. Spare CPU is shared.
- When a node runs low on memory, commands are frozen and swapped out instead of killed, then resumed later.
- A controller queues commands, places each workspace on the node with the warmest build, delegates GitHub credentials centrally, and serves a web cockpit on your tailnet.

## Use

All machines must be on one Tailscale tailnet owned by one Tailscale user (peers are authenticated by Tailscale identity; there are no cluster tokens).

```sh
hotbed init /var/lib/hotbed                  # on the controller
hotbed serve /var/lib/hotbed
sudo hotbed worker init <controller>         # on a Linux worker (uses ~/.hotbed)
hotbed worker init <controller>              # on macOS (QEMU Ubuntu + sparse qcow2 ZFS in ~/.hotbed)
hotbed nodes                                 # on a worker: its controller, recorded by worker init
HOTBED_CONTROLLER=<controller> hotbed nodes  # on any other machine on the tailnet
```

Every command supports `--json` (structured envelope) and agents can start with `hotbed skills get hotbed`. See [docs/design.md](docs/design.md) for architecture notes.

## License

MIT — see [LICENSE](LICENSE).
