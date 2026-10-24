# The operator's memory

The operator keeps a long-term memory: people, projects, decisions, preferences, what was tried and why. The
orchestrator saves to it. You can read it and suggest additions; you never write it.

## When to recall

Before you start work on a person, a project or a decision the operator has history with, search for it:

```
fleetctl memory recall "ram build host"          # the 5 best matches
fleetctl memory recall "slate release plan" --limit 10 --json
```

Search in plain words, a few at a time ("who reviews slate PRs", "billing migration decision"). Treat what comes
back as context the operator gave you, and say in your report when it changed what you did. It can be out of
date: when it disagrees with the code or with your brief, the brief wins; mention the disagreement.

## How to suggest

When you learn something durable that the operator would want kept (a preference, a decision and its reason, who
owns what), suggest it:

```
fleetctl memory suggest "Ram prefers squash merges on slate-server" --why "he asked for it in review of PR 31"
```

One fact per suggestion, a sentence or two that stands alone without this conversation. The suggestion goes to the
orchestrator, which decides whether to save it; you hear nothing back. Do not suggest task progress, guesses, or
anything already in the code or the docs.

**Never put secrets in a suggestion**: no tokens, keys, passwords, connection strings or personal data the
operator did not ask to keep. The memory is read by every session the operator runs.

## Who may

The operator, their Claude sessions and the orchestrator always may. An agent may only while its current task was
given by the operator, one of their sessions or the orchestrator; on a task a colleague gave, both commands answer
403. Colleagues on the tailnet never may. fleetd decides this from its ledger; there is nothing to configure.

## From any Claude Code session

The same two tools are an MCP server wherever fleetctl works (a fleet host, a laptop on the tailnet, an agent):

```
claude mcp add --scope user fleet -- fleetctl mcp
```

Its tools are `memory_recall` (query, limit) and `memory_suggest` (fact, why), with the same rules.
