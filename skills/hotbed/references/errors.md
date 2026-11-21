# hotbed error codes

| Code | Meaning | What to do |
|---|---|---|
| `INVALID_ARGS` | A bad flag, argument, name or size | Fix the command; `hotbed COMMAND --help` |
| `NOT_FOUND` | No such session, repo, workspace or command, or the session ended | Check the name; `hotbed session start` for a new session; `hotbed add S REPO` for a workspace |
| `CONFLICT` | The session name is taken, or the workspace is still preparing (retryable) | Pick another, or leave it out for a random one |
| `REPO_UNREACHABLE` | The worker cannot read the repo's remote (retryable) | Check the URL in `hotbed repo ls` and the node's access to it, then retry |
| `WORKSPACE_FAILED` | The workspace's base failed to freshen, or its fork failed | Read the reason; fix the repo's freshen commands (`hotbed repo add`, or `hotbed repo rebuild` if incremental state broke) and `hotbed add` again |
| `SYNC_REFUSED` | The checkout cannot be synced: hidden edits (`skip-worktree`, `assume-unchanged`), a dirty submodule, or a missing base commit | Do what the suggestion says, then sync again |
| `CONTROLLER_UNREACHABLE` | No answer from the controller (retryable) | Check `tailscale status` and that the controller runs `hotbed serve` |
| `WORKER_UNREACHABLE` | The node holding the workspace is offline (retryable) | Wait for it, or end the session and start one elsewhere |
| `NOT_AUTHORIZED` | The caller is not on the tailnet, or another user owns it | Call from one of the cluster owner's machines |
| `ALREADY_INITIALIZED` | `init` or `join` on a directory that has a node | `hotbed serve DIR`, or another directory |
| `NOT_INITIALIZED` | `serve` on a directory without `init` or `join` | Run `init` or `join` first |
| `TAILSCALE_UNAVAILABLE` | No tailscaled here, or not logged in | `tailscale up` |
| `NOT_JOINED`, `CLUSTER_MISMATCH` | A worker the controller does not know | `hotbed join` again with a new directory |
| `PLATFORM_NOT_SUPPORTED` | A worker command on a machine that is not Linux | Run workers on Linux |
| `INTERNAL` | A bug or an unexpected failure | Report it with the message |
