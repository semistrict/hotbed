# Decisions

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

# Open questions
