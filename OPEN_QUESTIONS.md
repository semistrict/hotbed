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

# Open questions

## May event-stream land on main and be pushed, then deployed?

The brief said to commit on the branch and not push; AGENTS.md says to land on main and push.
Deploying the branch without landing it would raise the controller's schema to 3, and the next
deploy of main (schema 2) would crash-loop the controller until the branch is deployed again.
bench-mode is deployed from its own branch the same way. Until main has both, a deploy of either
takes the other's features off the fleet.
