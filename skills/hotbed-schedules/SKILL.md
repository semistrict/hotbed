---
name: hotbed-schedules
description: Run recurring hotbed commands on a cron schedule, each run a session of its own, and look into their runs. Use when work should run by itself, such as a nightly benchmark.
license: MIT
---

# Scheduled commands

Recurring work, such as a nightly measurement, runs by itself:

```sh
hotbed schedule add nightly -r app --cron '0 2 * * *' --arch amd64 \
  --env BENCH_ENV=nightly -- scripts/nightly.sh
hotbed schedule show nightly                # the command, the next run, its runs and their sessions
hotbed schedule run nightly                 # a run now; prints its session, for hotbed poll -s and logs
```

- **Each run is a session of its own**, `NAME-YYYYMMDD-HHMM`, with a fresh workspace of the repo
  at its branch's head, in which the command runs detached at `/work/REPO`. It takes `run`'s
  flags (`--env`, `--mem-low`, `--mem-high`, `--kind`, `--arch`, `--runtime`). Nothing persists between runs except what
  the command keeps outside the workspace (such as what it pushes to git).
- **Its session's end.** A run is never abandoned while its command runs. One that succeeds ends
  its session. One that fails keeps it for the idle hour, so you can `hotbed run -s SESSION -r REPO --
  ...` in its workspace to look; its log stays (`hotbed logs COMMAND`).
- **Cron** has five fields in the controller's time zone (`CRON_TZ=Europe/Lisbon 0 2 * * *` for
  another). Runs never overlap: a slot that comes while the previous run is still going is
  `skipped`. A slot that passes while the controller is down is `missed`, not run late.
- `schedule add` with an existing name replaces it. `schedule ls [-p PROJECT | --all-projects]`
  lists the active project's schedules by default. `schedule pause`, `resume`, `rm`.
