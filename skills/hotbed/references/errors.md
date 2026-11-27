# hotbed error codes

| Code | Meaning | What to do |
|---|---|---|
| `INVALID_ARGS` | A bad flag, argument, name or size, or no node fits the run's `--arch`/`--bench`/`--mem-low`/`--vm-cpus`/`--vm-mem` | Fix the command; `hotbed COMMAND --help`; `hotbed nodes` for what each node has |
| `NOT_FOUND` | No such session, repo, workspace, command or scheduled command, or the session ended | Check the name; `hotbed session start` for a new session; `hotbed run` gives it a workspace |
| `CONFLICT` | The session name is taken, the workspace is still preparing (retryable), `schedule run` while a run is in progress, or the session has workspaces of the repo on several nodes | Pick another name, or leave it out for a random one; for a run, `hotbed poll -s SESSION` on the session the message names; pass `--node` to say which workspace |
| `REPO_UNREACHABLE` | The worker cannot read the repo's remote (retryable) | Check the URL in `hotbed repo ls` and the node's access to it, then retry |
| `WORKSPACE_FAILED` | The project's first base failed at a step, or the fork failed | Read the message, which names the step; fix it with `hotbed step add` (or `hotbed project rebuild PROJECT` if incremental state broke) and run again |
| `STEP_FAILED` | `step add`: the script failed in your workspace; it was not saved | Read its output (`hotbed logs CMD`), fix the script, add it again |
| `STEP_NOT_IDEMPOTENT` | `step add`: the script worked once but failed when run again; it was not saved | Make it safe to repeat (check before installing, `mkdir -p`, `--force`) and add it again |
| `SYNC_REFUSED` | The checkout cannot be synced: hidden edits (`skip-worktree`, `assume-unchanged`), a nested repo no `.gitmodules` names, or a missing base commit | Do what the suggestion says, then sync again |
| `CONTROLLER_UNREACHABLE` | No answer from the controller for the three minutes every call waits out a restart | It is down, not restarting: check the controller's HTTPS URL (`HOTBED_CONTROLLER`, or `hotbed login`) and the Cloudflare Worker's status. Every call, `run`, `sync`, `logs` and `stop` too, goes through it |
| `WORKER_UNREACHABLE` | The node holding the workspace, or the only one that fits the run, is offline (retryable) | Wait for it; a run from your checkout can go to any node, or `hotbed workspace release` the workspace to start over elsewhere |
| `COMMAND_STOPPED` | `run` or `logs -f`: someone stopped the command by hand; the message says who, their comment, and how to read any diagnostics taken | Read the diagnostics (`hotbed diagnostics CMD`) if taken, and act on the comment |
| `NOT_AUTHORIZED` | The sign-in or token is missing, expired, revoked, or for another organization (WorkOS sign-in, or an `hbj_`, `hbw_` or `hbu_` token) | `hotbed login` again, or pass a valid token (`HOTBED_TOKEN`; mint join and API tokens on the cockpit's Organization page) |
| `ALREADY_INITIALIZED` | `join` or `worker init` on a directory that already holds a node, or one joined to another controller | Use another directory, or `hotbed worker destroy` first |
| `NOT_INITIALIZED` | No controller is configured on this machine, or no worker in the state directory | `hotbed login https://CONTROLLER` or set `HOTBED_CONTROLLER`; on a worker, `hotbed worker init` or `join` first |
| `NOT_JOINED`, `CLUSTER_MISMATCH` | A worker the controller does not know, or one of another cluster | `hotbed worker init` again (new directory, with a fresh join token) |
| `PLATFORM_NOT_SUPPORTED` | The operation does not exist on this worker's platform (Linux, or Apple-silicon macOS with a native `macworker` and `machined` VMs), such as `worker trim` on a Mac, or a guest-agent operation an old VM image lacks | Run it where it exists |
| `INTERNAL` | A bug or an unexpected failure | Report it with the message |
