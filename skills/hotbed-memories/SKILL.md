---
name: hotbed-memories
description: Read the memories a hotbed session start lists, and save, update or retire one when a lesson has no better home (a preference, a machine fact, a workaround for an open bug). Use when a listed memory's summary applies, or you learned something every agent should know.
license: MIT
---

# Memories & learning

Memories are lessons shared across every agent harness and machine. Each is like a skill: a
**summary**, one line saying what it is about and when to read it, and a **body**, the memory
itself. Every `hotbed session start` (and `hotbed mcp`'s instructions) lists each active memory's
summary; read a body with `hotbed memory get` once its summary applies to what you are doing. New
memories are approved and active at once; the user can edit any field, promote a project memory to
global, archive and restore, reorder, or remove them in the cockpit or CLI.

```sh
hotbed memory get zsh-and-macos-shell-footguns   # a memory's body (several at once: SLUG..., global/SLUG, PROJECT/SLUG, #ID)
hotbed memory ls [-q QUERY] [--scope global|project] [--all-projects]
hotbed memory add --global \
  --slug short-replies \
  --summary "How long a chat reply may be: read before answering the user." \
  --body "Keep chat replies to a few scannable lines, answer first; put long detail in files or artifacts." \
  --rationale "User correction: they don't read long replies"
hotbed memory add --slug zfs-base-freshen \
  --summary "ZFS clone/snapshot base freshening internals: read before debugging base failures." \
  --rationale "Hit ZFS clone flake while debugging base freshening" \
  --body-file - <<'EOF'
...detailed markdown playbook...
EOF
hotbed memory add --supersede 4 --slug short-replies --summary "..." --body "..." --rationale "..."
hotbed memory edit 4 --summary "..." [--body "..."] [--global | -p PROJECT] [--repo REPO] [--rationale "..."]
hotbed memory promote 2                          # promote a project-specific memory to global
hotbed memory archive 2 --reason "..."           # soft-retire (restorable via `memory restore`; --reason required for agents)
hotbed memory rm 2                               # permanently remove a memory
```

- **A memory is the last resort.** Every memory's summary costs every session's context, so
  first put the lesson where it will be found: fix the bug or tooling quirk itself (a script, a
  check, a clearer error); write a design decision as a comment beside the code it constrains, or
  in the repo's `AGENTS.md` (a subdirectory's own `AGENTS.md` for rules about that part); and put
  how to use a tool in that tool's skill. Save a memory for what has no such home: the user's
  preferences, facts about machines, and workarounds for open bugs (name the bug's task so the
  memory can go once it is fixed). Check `hotbed memory ls -q WORD` first and update a memory that
  already covers it; keep each memory to one rule, so one can be retired without the others.
- **Write the summary as its trigger.** The summary (at most 280 characters) is all a session sees
  until it reads the body, so say when it applies ("read before debugging base failures"), not
  only what it is. Both summary and body are required.
- **Save lessons when you resolve them (not just in local files):** when you resolve a
  non-obvious bug or tooling quirk that has no better home, the user corrects you, or the user says
  "remember this" / "add to memory" / `/learn`, save it with `hotbed memory add` (alias
  `memory propose`, MCP: `memory_add`). Agents must pass `--rationale "..."` saying why it is
  being added and what triggered it; the rationale is metadata only, never returned when reading
  memories. `hotbed session end` prompts you at wrap-up.
- **Archive outdated memories with a reason:** when a memory is obsolete or no longer true,
  soft-retire it with `hotbed memory archive ID --reason "..."` (MCP: `memory_archive`; `--reason`
  is required for agents). `hotbed memory restore` brings one back.
- **Two scopes:** defaults to your session's project (and repo if held by the project); pass
  `--global` for cross-project preferences and workflows, or `-p PROJECT` for a lesson that
  belongs to another project than your session's. `hotbed memory promote ID` (or
  `hotbed memory edit ID --global`) promotes a project memory to global. A project's memory can
  supersede only global memories and its own project's; a global one can supersede any.
- **Update & condense, don't duplicate:** if a slug already exists, hotbed tells you its `#ID` so
  you can pass `--supersede ID` or edit in place with `hotbed memory edit ID` (supporting
  `--clear-title`, `--replace-tags`, `--replace-content`, `--repo`, `--rationale`).
  `hotbed memory show REF` prints provenance, supersession links, and full revision history.
- **Startup caps:** a session start lists at most 40 summaries in 8 KiB, in the user's order; it
  says how many it left out (`hotbed memory ls` lists them all).
