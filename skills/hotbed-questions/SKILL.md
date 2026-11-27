---
name: hotbed-questions
description: Ask the user a decision on hotbed's question list instead of stopping to wait, record a decision taken without asking, and act on answers as they arrive. Use when work needs the user's call, or hotbed poll brings an answer.
license: MIT
---

# Asking the user: questions

When you need the user's decision, don't stop and wait: ask on the project's question list, go
ahead with your best guess, and read the answer when it comes. A question takes the JSON
AskUserQuestion takes, plus a little; each of its questions becomes an entry of its own:

```sh
hotbed question add <<'EOF'
{"questions": [{"question": "Which store should sessions use?", "header": "Store", "multiSelect": false,
  "body": "Sessions outgrew `sessions.json`: **4 MB**, rewritten on every change.",
  "options": [{"label": "SQLite", "description": "One file, already used", "implemented": true},
              {"label": "Postgres", "description": "A server to run"}]}]}
EOF
hotbed poll                      # the answer arrives as an event, the question with it; keep working meanwhile
hotbed question outbox           # answers waiting for an agent
hotbed question ack 12           # you read it and acted on it: it is handled
```

- **Send the user its link.** Each question `question add` returns carries its cockpit page
  (`cockpitUrl`, printed under it), where the user answers it: when you want an answer, put the
  link and the question in plain words in your reply, not just its number.
- **Poll before you assume no answer.** The user answers whenever they like; an answer reaches
  you only through `hotbed poll`. Before you end your turn, commit, or conclude the user has not
  answered, poll. Every question command's response carries a `reminder` saying so.
- **Say what you went ahead with.** Mark the option you built `"implemented": true`: going ahead
  and marking it is the norm, so the user sees what changes if they answer otherwise. Mark
  `"recommended": true` explicitly for a way forward you have not built: option labels are
  delivered verbatim (no `"(Recommended)"` suffix stripping). One of each at most unless
  `multiSelect`; never both on one option.
- **Record a decision you took without asking** with `question add --answered`: the same JSON,
  what you decided marked `"implemented": true` (required) and what else was possible as the
  other options, why and how to undo it in `body`. It is answered with its implemented options and
  handled at once, by your session: never in the outbox, waking no one. The user reopens and
  answers it to overturn it, which reaches the outbox as any answer does.
- **Keep the title short.** `question` is one sentence; the setting, findings and stakes go in
  `body`. Title, body, labels and descriptions are markdown; `preview` is shown as it is.
- **One list per project, every session's.** `question ls` lists your project's questions, newest
  first (`--sort oldest|updated|rank`, `--state open|answered|dismissed|handled|deleted|all`,
  `--asked-by SESSION`, `--t3code-thread-id ID`, `-p PROJECT`, `--all-projects`). Read it before
  asking: another session may have asked already.
- **Sessions and T3 Code threads are mapped on every question.** Each question carries its
  `t3codeThreadId` (captured from `T3CODE_THREAD_ID` when asked or resolved from its asking
  session), and `question outbox` / `question ls` also return a `sessions` summary (session ID,
  project, liveness, `t3codeThreadId`, matched `questionIds`, and state counts) so you can peek at
  the outbox (`hotbed question outbox --all-projects`) and immediately see which sessions and T3
  Code threads have answers waiting.
- **The user answers in the cockpit**, or dismisses (no answer will come). An answer waits in the
  outbox until an agent acknowledges it; any agent of the project may act on and ack an answer.
  An answer or dismissal is also an event of the asking session: its `hotbed poll` returns it,
  with the question as answered.
- **Any session may edit (`question edit ID`, while open) or delete (`question rm ID --reason`)
  any question** it finds wrong or no longer needed. Nothing is lost: `question show ID` has the
  whole history, each change with its session and time.
- A question records where you asked from (checkout, branch, commit, changed files) and, if the
  project holds your checkout's repo, that it is about that repo (`--repo REMOTE` names another).
- A session changes only its own project's questions (edit, rm, ack, answer, dismiss, revise,
  reopen, move).
- The user may change a response still in the outbox, return any answered or handled question to
  the inbox (`question reopen ID`, which clears its response) and revise any question
  (`question revise ID`, JSON as `question edit` takes): read the outbox again before acting.
- Never answer a question yourself unless the user told you the answer: then pass it on with
  `question answer ID --pick LABEL [--other TEXT] [--notes TEXT] -s SESSION`, or `question dismiss
  ID --reason WHY`; `question reopen` and `question revise` likewise only at the user's word.
  `question move ID --first|--last|--before ID` reorders the user's list within the question's
  project. `question projects` counts every project's open and outbox questions.
