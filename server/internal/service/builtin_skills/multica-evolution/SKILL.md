---
name: multica-evolution
description: "Use when the Evolution agent analyses a scheduled run's signals, writes its report, attributes a recurring problem with an agent, workflow, or team setup, or drafts an improvement proposal. Covers the report format, the attribution method, the proposal format, and the guardrails."
user-invocable: false
allowed-tools: Bash(multica *)
---

# Propose an improvement

A proposal asks a workspace owner to change one thing — an agent, a workflow,
or how the team is organized — and shows why. The owner decides; you never
apply the change.

## Report on an analysis run

An analysis run ends with one comment on its analysis issue:

````
## Window
Sources read, the time range, how many items, and whether the run's item
budget was reached (the rest goes to a catch-up run).

## Patterns
- One sentence per pattern: what keeps happening, where, what it costs.
  Instances: issue identifiers or comment links; chats as `chat <session id>`
  without quoting. Mark each statement as fact or inference.

## Proposals
- Issue identifier · target · the change in one line · created, merged into
  an open proposal, or refused (with the server's reason).

## Nothing found
Say so plainly when the window holds no pattern worth a proposal.
````

- A pattern needs at least two independent instances; one is an anecdote.
- Report what the signals show, not what the workspace's configuration says.
- Keep it short: the report is read by an owner deciding what to look at.

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

## File a proposal

Write the proposal as JSON and file it with
`multica evolution propose --file proposal.json`:

```json
{
  "target": {"type": "agent", "id": "<agent uuid>"},
  "field": "instructions",
  "topic": "handoff after review",
  "title": "Say who acts after a review is rejected",
  "problem": "Facts cite evidence; inferences are marked as such.",
  "change": {"before": "current text, copied exactly", "after": "proposed text"},
  "evidence": [
    {"type": "comment", "id": "<comment uuid>", "note": "what it shows", "quote": "optional excerpt"},
    {"type": "chat", "id": "<chat message uuid>", "note": "a summary, never the message itself"},
    {"type": "issue", "id": "<issue uuid>", "note": "what it shows"}
  ],
  "expected_effect": "Which signal should move, in which direction, over how long.",
  "risk": "low",
  "risk_reason": "Why this tier.",
  "rollback": "How to undo it."
}
```

The server writes the proposal issue (sections: target, problem and cause,
change as a diff, evidence, expected effect, risk) and labels it
`evolution`. It refuses, with a `code` and a reason, when:

- `field` is not on the target's whitelist — agent: `instructions`,
  `description`, `skills`; workflow: `steps`, `name`, `description`; squad:
  `instructions`, `description`, `members`, `leader`;
- the target is you, an archived or internal agent, or an excluded agent;
- `change.before` is not in the target's current text (copy it from the
  target as it is now; leave it empty for a pure addition). For `skills`
  the current text is the enabled skill names, one per line; for `steps`,
  the workflow's steps as indented JSON;
- the evidence comes from fewer independent sources (different issues or
  chat sessions) than the workspace requires, or cites something you may
  not read;
- a quote is on a chat or an issue, is longer than 120 characters, or is not
  an exact excerpt of the comment;
- any text repeats a chat message word for word (20 characters in a row),
  or copies more than 120 characters of a comment in a row — whether you
  cite it or only read it in this run;
- any text names a workspace member, or contains what looks like a secret,
  credential, email, phone number, internal host, or URL query string;
- this run already opened its maximum number of proposals;
- the same proposal was declined recently.

The same target, field, and topic is the same proposal. When one is still
open, the server adds the evidence it does not cite yet as a comment
(`"outcome": "merged"`, or `"duplicate"` when there is nothing new) instead
of opening another issue. Keep the topic stable across runs so this works.

- Change only what the problem needs. Prefer deleting or merging lines over
  adding them.
- One proposal per change. Do not bundle unrelated changes.

## Guardrails

- Never target yourself.
- Never touch permissions, secrets, credentials, environment variables, MCP
  configuration, runtimes, or visibility — not even as a suggestion inside a
  larger change.
- Creating, merging, or re-routing agents and squads is proposal-only: describe
  the change for a person to carry out.
- Never paste chat messages; summarize them and point to the chat session.
  From comments, quote at most one excerpt of up to 120 characters. Never name
  members; say "a member" or describe their role. Remove anything resembling a
  secret.
- File proposals only with `multica evolution propose`; never create proposal
  issues or comment on them yourself. Mention every proposal in your run's
  report.
