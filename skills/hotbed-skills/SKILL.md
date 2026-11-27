---
name: hotbed-skills
description: Store, install, read and update Agent Skills in hotbed (global or per project), npx-skills compatible, and serve them over MCP. Use when adding a skill, installing skills into an agent, or reading one a session start listed.
license: MIT
---

# Agent Skills (CLI, `npx skills`-compatible install, & MCP)

Hotbed stores reusable [Agent Skills](https://agentskills.io) (multi-file `SKILL.md` packages with `references/`, `scripts/`, `assets/`) in the controller, scoped either globally or to a project/repo, and announces **every** active skill descriptor relevant to your session on `hotbed session start` (pass `--omit-skills` to suppress). Skills are also served natively over MCP via the **MCP Skills Extension (SEP-2640, `io.modelcontextprotocol/skills`)** and standard MCP `resources/*` (`skill://global/<name>/SKILL.md`, `skill://<project>/<name>/SKILL.md`, `skill://builtin/<name>/SKILL.md`).

hotbed's own guides (`hotbed`, `hotbed-questions`, …) are **built-in skills**: compiled into the
CLI, listed first on every session start (`[builtin]`), read offline with `hotbed skills get NAME`,
and never stored on the controller. Their source is `skills/` in the hotbed repo.

```sh
# Install from any `npx skills` source (GitHub shorthand, @skill, @ref, GitHub/GitLab/Git URL,
# local dir, RFC 8615 well-known URL, or direct SKILL.md / .zip / .tar.gz archive):
hotbed skills add vercel-labs/agent-skills --list            # preview skills in source
hotbed skills add vercel-labs/agent-skills -s frontend-design -g
hotbed skills add owner/repo@my-skill -p myproj              # project-scoped skill
hotbed skills add ./local-skills --rename custom-name        # rename SKILL.md frontmatter on install
hotbed skills add owner/repo --local -a claude-code -a codex # also install into local agent dirs
hotbed skills install --from-hotbed -g -a claude-code        # materialize active hotbed skills locally

# Inspect, read, search, update, and manage skills:
hotbed skills ls [-q QUERY] [--scope global|project] [--all-projects]
hotbed skills get NAME [SUPPORTING_FILE] [--all-files]       # e.g. `hotbed skills get code-review references/checklist.md`
hotbed skills show NAME                                      # manifest, SHA-256 digests, upstream source, history
hotbed skills find QUERY [--owner USER]                      # search skills.sh
hotbed skills check [NAME...]                                # check upstream sources for updates
hotbed skills update [NAME...]                               # pull latest upstream changes into hotbed
hotbed skills init [NAME]                                    # scaffold a new SKILL.md locally
hotbed skills edit REF [--global | -p PROJECT] [--tag TAG] [--rationale "..."]
hotbed skills promote REF                                    # promote project skill to global
hotbed skills archive REF [--reason "..."] / restore REF / rm REF / move REF --first|--last|--before ID
```

- **`hotbed skills install` (no arguments)** still installs the built-in skills into `~/.claude/skills` offline (used by `hotbed upgrade`). Passing a source (`hotbed skills install owner/repo`) delegates to `skills add`, matching `npx skills install`.
- **Cockpit `/skills`:** browse, inspect every supporting file, create new skills, promote, archive/restore, reorder, and delete skills from the web UI.
