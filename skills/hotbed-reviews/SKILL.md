---
name: hotbed-reviews
description: Ask reviewer agents (and the user) to review an exact version of a change through hotbed, review one as a reviewer, and record each finding's decision. Use when a change needs review, or you were given a hotbed review number.
license: MIT
---

# Reviews: asking reviewer agents, and the user

A review is of an exact version: the commits it names never move. Both the implementer's exact model
and every reviewer's exact model are recorded; multiple reviewers (in separate sessions or as
sub-agents sharing one session) can review the same review. Ask for one in the checkout to review,
then give its number to one or more reviewer agents:

```sh
hotbed review request --model gemini-4-neon --title "Retry on lost leases" --instructions "look for races"
                                 # --model names the implementer's exact model (or inherited from `session start --model`)
                                 # base: where HEAD left origin's default branch (--base REF, --whole)
                                 # head: HEAD, or a snapshot commit of uncommitted changes, pinned
hotbed poll                      # each reviewer's end arrives as an event; keep working meanwhile
hotbed review show 7             # implementer, reviewers, findings (with locations, decisions, quick_by and responses)
hotbed review accept 7 1 3 --reason "fixing"
hotbed review dismiss 7 2 --reason "pre-existing; not this change"   # alias: reject
hotbed review wont-fix 7 4 --reason "intentional tradeoff; documented"
hotbed review fixed 7 1 --commit HEAD --where lease.go:80-95 --notes "renew holds the lock"
                                 # handled: the commit in full, the files and notes optional;
                                 # the user can respond to it no more (reopen if the fix fails)
hotbed review invalid 7 3 --evidence "expiry checks the clock under the lock (lease.go:41)"
                                 # not a real defect, with Markdown evidence; the user may dispute it
hotbed review link 7             # the review's page in the cockpit: send it to the user
hotbed review responses 7        # the responses since you last read them (poll says when one comes)
```

Each reviewer (in its own session, or as a sub-agent sharing the parent session):

```sh
hotbed review start 7 --model claude-opus-4-6 [--subagent verifying-reviewer]
                                 # claim/join the review, naming your exact model ID (required);
                                 # multiple models or sub-agents may start on the same review
git diff BASE HEAD               # the commits are in the requester's checkout (refs/hotbed/reviews/HEAD)
hotbed review findings 7 [--reviewer 1] <<'EOF'
{"findings": [{"short_summary": "Lease renewal races expiry", "summary": "...", "failure_scenario": "...",
  "category": "correctness", "severity": "important", "verdict": "CONFIRMED", "file": "lease.go", "line": 88,
  "locations": [{"path": "renew.go", "start_line": 12, "end_line": 20, "excerpt": "..."}]}]}
EOF
hotbed review finish 7 [--reviewer 1] --summary "One race, one nit."   # or: hotbed review fail 7 --reason WHY
```

- When multiple reviewers are active in the same session (e.g. concurrent sub-agents sharing
  `T3CODE_THREAD_ID` / `HOTBED_SESSION`), pass `--reviewer ID` (or `--model` / `--subagent`) to
  `findings`, `finish` and `fail` so each finding and report is attributed to the right reviewer.
- A finding is ReportFindings' JSON (severity `important`, `pre_existing` or `nit`; verdict
  `CONFIRMED`, `PLAUSIBLE` or none), with any number of `locations` besides, or none. Put the
  lines in `excerpt`: the cockpit shows them without the code. Found nothing? Report
  `{"findings": []}`: the review's events say so.
- A reviewer that cannot reach hotbed (in a network-isolated sandbox, such as `codex exec
  --sandbox read-only`) ends its report with its findings in a fenced block whose info string is
  `json hotbed-findings`, holding that same `{"findings": [...]}` (`[]` when none survive). Its
  caller records it with `--model` (and `--subagent` if sharing a session), and the review keeps
  the whole report for the cockpit:

  ```sh
  hotbed review start 7 --model gpt-5.5-codex --subagent codex-exec
  codex exec --sandbox read-only --output-last-message report.md - < instructions.md
  hotbed review findings 7 --model gpt-5.5-codex --from-report report.md
  hotbed review finish 7 --model gpt-5.5-codex --summary "$(head -1 report.md)"
  ```
- Every decision and response explicitly records `actor_kind` (`USER` vs `AGENT`) plus the agent's
  exact `model`, `session`, optional `subagent`, and `t3code_thread_id`. `hotbed review respond`
  uses your bound agent session by default; pass `--as-user` only when recording the human user's
  explicit word. On findings, `quick_by` shows whether the user clicked `Verify` / `Fix if verified`
  / `Won't fix` or an agent set it.
- `hotbed review ls` lists the project's reviews (the session's, or the one holding this checkout's
  repo); `review cancel` withdraws one; `review reopen`
  makes a decided finding open again. Nothing is removed: `review show` has every event.
