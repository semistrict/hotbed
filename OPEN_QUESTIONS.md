# Decisions

## Projects and questions (board 6434)

- **A repo no project holds becomes a project of its own name** when a session first starts in
  it, so `session start` in a checkout keeps working without setup. The alternative was to refuse
  until someone runs `hotbed project add`. To undo: make `sessionProject` return an error in the
  "no project holds it" case.
- **A project may hold no repos** (`project add NAME` alone), for work that is not in a repo yet,
  and the tests' project. Requiring one repo is a check in `AddProject`.
- **Sessions from before projects keep no project** (`sessions.project_id` NULL); they cannot ask
  questions and say to start a new session. Nothing migrates them: they end within the idle hour.
- **Question IDs are one sequence across projects** (`question 12`), so commands that name a
  question need no project. Per-project numbers would need a project on every such command.
- **"handled"** names an acknowledged answer (over "archived"): it says an agent dealt with it.
- **A dismissal goes to the outbox too**, so the asker learns no answer will come; acking it is
  the same as acking an answer.
- **Only open questions can be edited, moved, answered or dismissed.** A user who changes their
  mind after answering, or an agent who wants to re-ask, adds a new question; the old one's
  history stays. Allowing re-answers would mean deciding what an agent that already acked sees.
- **Questions belong to their asking session's project**; the repo they are about is the asker's
  checkout when the project holds it, else the whole project unless `--repo` names one.
- **A session edits, deletes and acks only its own project's questions.** "Any session may
  delete any question" holds within a project, whose sessions share the list; an agent of another
  project acking an answer would take it from the sessions that read that outbox. To lift it,
  drop `mustBeOurs` in internal/controller/questions.go.
- **`question wait` with no IDs waits for an answer to the session's own questions**
  (`--project-wide` for any), so an answer another session leaves unacked does not end every
  wait at once. A cursor over the outbox was the alternative.
- **A change the user makes records a session only when named (`-s`, HOTBED_SESSION)**, never a
  session merely bound to the thread or worktree, which may have ended long ago.
- **A repo's own project is named from its remote's last part, made a valid name** (Next.js →
  next-js); a project of that name that holds no repo takes the repo in.
- **A schedule keeps the project it was added in** (`schedule add -p`, or its repo's then), so a
  repo another project takes in later does not stop its runs. Schedules added before keep
  resolving by repo, and say to re-add with `-p` when that is ambiguous.
- **The user's ordering is one rank across all questions**; `move --first` puts a question first
  in its own project's list.
- **The chosen project in the cockpit is per viewer**, kept in the browser (localStorage), not in
  the URL. Putting it in the URL would make views shareable at the cost of threading it through
  every link.

## The workspaces page drops its fork graph (2026-11-21)

Ramon found the per-repo fork graph (bases as squares, forks as sprouts, one lane per node) not
useful and wanted the workspaces themselves first. The page now leads with the table, filtered by
repo chips and a search box (`?repo=`, `?q=`), and the "owned storage by repo" chart went with the
graph. Keeping the graph lower on the page was the alternative. To bring it back, restore
`ui/src/components/charts/ForkTree.tsx` from git history and render it under the table.

## Client commands take their controller from worker init, never a flag (2026-11-21)

`--controller` is gone from every client command. They call the controller `hotbed worker init`
recorded in `~/.hotbed` (`vm/config.json` on macOS, `setup.json` on Linux). `HOTBED_CONTROLLER`
stays, and wins, for machines that are no worker (laptops, ubu1 when its worker was set up with
`hotbed join`, scripts): without it they could not call the cluster at all. To drop it, delete
the env read in `localController` (cmd/hotbed/main.go) and give such machines another way to
record a controller.

## The event stream keeps old workers working (2026-11-21, branch event-stream)

AGENTS.md says no migrations or compatibility shims. The brief for the event stream asked for a
migration "with no flag day: the cluster runs agents' work right now", so this change keeps
three: the controller still takes `Report` from workers that do not push events, an upgraded
worker delivers its old outbox once through `Report`, and schema migration 3 copies each node's
last report into its view. Without them, a deploy would lose the session events and history of
commands that end during it, and old workers would drop out until upgraded.

Undoing it, once every worker pushes events: delete `Server.Report`, `Store.Ingest`, the worker's
`legacyReport`/`drain`/outbox table, and the `report` copy in migration 3; reserve the proto
fields.

## Updates: CI builds, the controller follows, everything follows the controller (board 6318)

- **The controller reads releases with the user's own GitHub token,** from their gh login on ubu1
  (`gh auth token`), kept root-only in `/var/lib/hotbed/controller/github-token` by the host's
  setup step (155-hotbed.sh). The repo is private, so the controller needs a token; this one
  already lives on that machine, so nothing new is exposed there. It has more scope than reading
  releases needs. The alternative is a fine-grained token with read access to this repo's
  contents. To switch, write it to that file; the setup step would then need to stop rewriting
  the file.
- **A dev build on the controller is replaced within two minutes** by CI's newest. Branches are
  tried in hotbed sessions (AGENTS.md), never deployed to the fleet's own nodes, so nothing holds
  a build back. To undo, add a hold to the controller's `updates`, for example a pinned build.
- **Workers run exactly the controller's build,** newer or older. A controller moved back to an
  older build takes every worker back with it.
- **Clients follow only when they run a CI build.** A dev build (someone building hotbed) is left
  alone; `hotbed upgrade` installs the controller's build into it on demand.
- **No automatic rollback.** A build that crashes at startup stays broken until a newer one, or a
  release installed by hand with the fleet skill's `hotbed/deploy.sh`. CI's full test run gates
  every release.
- **CI keeps the newest 20 releases.**

## Every API call has a CLI command (2026-11-21)

- **Worker-to-controller calls are exempt** (Join, Report, PushEvents, Heartbeat): `hotbed join`
  and `hotbed serve` make them for the node; a command for an agent to make them by hand would
  only let it impersonate a worker. To change, drop them from `nodeProtocol` in
  `cmd/hotbed/coverage_test.go` and add commands.
- **The check runs only on a whole-suite run** (no `-run`/`-skip`), from `TestMain`: a filtered
  run cannot tell an untested call from one it did not run.
- **New commands:** `hotbed workspace ls|show|files|file|release`, `hotbed logs -n N`
  (GetLogLines), `hotbed snapshot` (GetSnapshot). `workspace release` goes through the
  controller, which keeps the session's account of its workspaces, not straight to the worker.

# Open questions
