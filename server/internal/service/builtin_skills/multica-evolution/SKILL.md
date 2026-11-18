---
name: multica-evolution
description: "Use when the Evolution agent attributes a recurring problem with an agent, workflow, or team setup, or drafts an improvement proposal. Covers the attribution method, the proposal format, and the guardrails every proposal must pass."
user-invocable: false
allowed-tools: Bash(multica *)
---

# Propose an improvement

A proposal asks a workspace owner to change one thing — an agent, a workflow,
or how the team is organized — and shows why. The owner decides; you never
apply the change.

## Attribute before you propose

1. Collect the instances. A single failure is an anecdote; a proposal needs
   independent instances of the same problem (different issues, different
   days, or different people).
2. Name the pattern in one sentence: what keeps happening, where, and what it
   costs (rework, waiting, a wrong result, a repeated question).
3. Find the cause you can change. Ask in order:
   - Is the instruction missing, wrong, ambiguous, or contradicted elsewhere?
   - Is the workflow step unclear about who acts next or what "done" means?
   - Is the work routed to the wrong agent, or to no one?
   - Is it outside configuration entirely (a flaky tool, a missing
     permission)? Then it is not a proposal — say so and stop.
4. Keep facts and inferences apart. A fact links to its evidence; an
   inference says what it is drawn from.

## Proposal format

Write each proposal with these sections, in this order:

````
## Target
agent | workflow | squad · <name> · <id>

## Problem and cause
One paragraph. Facts cite evidence; inferences are marked as such.

## Change
```diff
- current text
+ proposed text
```

## Evidence
- <link> · <date> · one short excerpt

## Expected effect
Which signal should move, in which direction, and over how long.

## Risk
low | medium | high, the reason, and how to undo the change.
````

- Show the exact current text in the diff, copied from the target as it is
  now. A diff against text you remember rather than read is not reviewable.
- Change only what the problem needs. For agents that is instructions,
  description, or skill bindings; for workflows, steps, names, or
  descriptions.
- Prefer deleting or merging lines over adding them.

## Guardrails

- Never target yourself.
- Never touch permissions, secrets, credentials, environment variables, MCP
  configuration, runtimes, or visibility — not even as a suggestion inside a
  larger change.
- Creating, merging, or re-routing agents and squads is proposal-only: describe
  the change for a person to carry out.
- Quote people sparingly and remove personal details and anything resembling a
  secret from excerpts.
- Every proposal is a new issue. Before writing one, look for an open proposal
  about the same target and problem; if there is one, do not open a duplicate
  and do not write to the existing issue — mention it in your run's report.
