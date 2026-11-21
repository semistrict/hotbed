---
name: hotbed
description: Run commands on a hotbed cluster of Linux machines on a tailnet. Use when an agent needs to set up a cluster, add a worker, or check which workers are up.
---

# hotbed

hotbed runs agents' builds, tests and dev servers on a cluster of Linux machines on one tailnet.
One machine is the controller; every other one is a worker. Every node, and every client, runs the
same `hotbed` binary.

## Output

Every command except `run` prints one JSON envelope to stdout and nothing else:

```json
{"version":"1","ok":true,"command":"nodes","data":{}}
{"version":"1","ok":false,"command":"join","error":{"code":"CONTROLLER_UNREACHABLE","message":"...","suggestion":"...","retryable":true}}
```

- Read `ok` first. On failure, act on `error.code`, never on the message text.
- `error.retryable: true` means the same command, unchanged, can succeed later (the controller is
  restarting, say). `false` means something must change first; `error.suggestion` says what.
- Exit status: `0` success, `1` error envelope, `2` bad arguments (also an envelope, code
  `INVALID_ARGS`).
- `run` is a long-lived process. It logs JSON lines to stderr and prints an envelope when it stops.

## Setting up a cluster

```sh
# On the controller:
hotbed init /var/lib/hotbed      # data.address is what workers join
hotbed run /var/lib/hotbed

# On each worker (Linux only):
hotbed join ubu1 /var/lib/hotbed
hotbed run /var/lib/hotbed

# Anywhere on the tailnet:
HOTBED_CONTROLLER=ubu1 hotbed nodes
```

No tokens: the controller asks Tailscale which node and which user is calling, and admits nodes and
clients owned by the same tailnet user as the controller's own node. The controller listens only on
its tailnet addresses (port 7120 unless `init --port` says otherwise).

A worker counts as `online` while its last heartbeat is at most 15 seconds old.

## Error codes

| Code | Meaning | What to do |
|---|---|---|
| `INVALID_ARGS` | Bad flag, argument or address | Fix the command |
| `ALREADY_INITIALIZED` | The state directory already holds a node | Use it with `run`, or pick another directory |
| `NOT_INITIALIZED` | `run` on a directory without `init` or `join` | Run `init` or `join` first |
| `TAILSCALE_UNAVAILABLE` | No tailscaled, or not logged in | `tailscale up`; check `tailscale status` |
| `CONTROLLER_UNREACHABLE` | No answer from the controller (retryable) | Check the controller is running `hotbed run` |
| `NOT_AUTHORIZED` | The caller is not on the tailnet or has another owner | Call from a node of the cluster's owner |
| `NOT_JOINED` | A worker the controller does not know | `join` again with a new state directory |
| `CLUSTER_MISMATCH` | The controller was initialized again since the worker joined | `join` again with a new state directory |
| `PLATFORM_NOT_SUPPORTED` | A worker command on a machine that is not Linux | Run workers on Linux |
| `INTERNAL` | A bug or an unexpected failure | Report it with the message |
