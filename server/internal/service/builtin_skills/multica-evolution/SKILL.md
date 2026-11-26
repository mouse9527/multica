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

Only in a structural review (the analysis issue lists `metrics`), add after
Patterns:

````
## Structure
- One line per finding: the metric reference, its value and sample, what it
  suggests, and the proposal it led to — or why it led to none.
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
  `instructions`, `description`, `members`, `leader`; team: `new_agent`,
  `merge_agents`, `retire_agent`, `routing`;
- the target is you, an archived or internal agent, or an excluded agent;
- `change.before` is not in the target's current text (copy it from the
  target as it is now; leave it empty for a pure addition). For `skills`
  the current text is the enabled skill names, one per line; for `steps`,
  the workflow's steps as indented JSON;
- `change.before` occurs more than once in that text: quote enough of it to
  pick out one place, because an approved proposal is applied exactly where
  it points. For `steps`, the change must leave valid JSON;
- the evidence comes from fewer independent sources (different issues or
  chat sessions) than the workspace requires, or cites something you may
  not read;
- a `metric` item is cited outside a structural review, names a metric the
  window does not have, rests on a smaller sample than `min_effect_sample`,
  or its `value` is not the number the server recomputes;
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

## Propose a team change

Creating, merging, and retiring agents and routing work between agents and
squads are team proposals: `"target": {"type": "team", "id": "<workspace uuid>"}`,
`field` one of `new_agent`, `merge_agents`, `retire_agent`, `routing`,
`change.before` empty, and `change.after` the change for a person to carry out
(which agents or squads, what each should own; for `retire_agent`, which agent,
where its work goes instead, and what is lost). They need evidence like any other proposal.
Nothing applies them, and no apply button is offered: an approved team
proposal is carried out by hand. Squad proposals are text only in the same way.

## Structural analysis (structural review only)

This section applies only when the analysis issue lists the `metrics` source.
Otherwise skip it: do not read metrics and do not cite them.

Read `multica evolution signals --source metrics --days 14` once. Then look for
problems no single comment shows:

- **A step that keeps sending work back**: a high `return_rate`, or issues in
  `return_loops`. Ask why work reaches the step unready, and whether a step
  before it, or a check moved earlier, would catch it.
- **A column where work waits**: a high `dwell_p50_hours` or
  `dwell_p90_hours`, especially where many `entries` come from one place.
  Ask who it waits for and whether a rule could let some of it through.
- **A check that rarely changes the outcome**: a step that almost always
  exits forward (low `return_rate` on a large sample) while its agent's runs
  are long. Ask whether it could run on fewer issues, not whether to drop it.
- **An agent that is overloaded, idle, or overlaps another**: `runs`,
  `short_run_rate`, `failure_rate`, `triggers`, `handoffs_in` and
  `handoffs_out`. Many short runs with nothing to do suggest a lower cadence;
  two agents handing the same work back and forth suggest a merge.

A number is a fact; why it is high is an inference — mark it so, and back it
with comments or issues when you can. Then propose the smallest change: a
workflow `steps` change, a squad change, or a team `routing`, `merge_agents`,
or `retire_agent` proposal. Prefer narrowing a step or an agent's trigger to
removing it.

Cite each number as `metric` evidence:

```json
{"type": "metric", "id": "agent:<agent uuid>/short_run_rate", "value": 0.28, "note": "28% of its runs end in under 45 s"}
```

Metric ids:

- `workflow:<workflow uuid|default>/step:<status key>/<metric>`, metric one of
  `entries`, `exits`, `returns`, `return_rate`, `dwell_p50_hours`,
  `dwell_p90_hours`; `default` is the built-in Default workflow (an empty
  `id` in the summary);
- `workflow:<workflow uuid|default>/issues`;
- `agent:<agent uuid>/<metric>`, metric one of `runs`, `finished`,
  `completed`, `failed`, `cancelled`, `failure_rate`, `cancel_rate`,
  `duration_p50_seconds`, `short_runs`, `short_run_rate`, `handoffs_in`,
  `handoffs_out`;
- `issue:<issue uuid>/returns` for an issue in `return_loops`.

`value` is the number exactly as the summary gives it (rates are fractions,
not percentages). The server recomputes it over the same 14 days and refuses
the item when it differs, or when the number rests on fewer runs or issues
than `min_effect_sample`. The numbers of one workflow step, one agent, or one
issue count as one independent source.

Every structural proposal says, in `problem` and `expected_effect`, which
metric it rests on, its current value as the baseline, and the change you
expect over the next weeks: for example "`return_rate` of review 0.28
(sample 54) → below 0.15 within four weeks".

## Condense instructions (monthly review)

A condense review issue lists agents whose instructions are long or repeat
themselves, with their measurement. For each listed agent:

1. Read its instructions with `multica agent get <id> --output json`.
2. Rewrite the longest stretch you can say in fewer words: merge repeated and
   overlapping lines, cut filler and restated context. Keep every rule and its
   meaning; a condense proposal changes how the instructions read, never what
   the agent does.
3. File it with `"field": "instructions"`, `change.before` copied exactly (the
   whole instructions when they fit in 16,000 characters), the shorter
   `change.after`, and `"evidence": []`. The server sets the topic, attaches
   the measurement, and refuses a change that is not shorter
   (`change_not_shorter`), an agent the review does not list, or any evidence.
4. One condense proposal per agent is kept open: filing again while it is open
   adds nothing (`"outcome": "duplicate"`), and a declined one is not filed
   again during the cooldown.

The approved proposal is applied, and can be rolled back, like any other agent
instructions proposal.

## Guardrails

- Never target yourself.
- Never touch permissions, secrets, credentials, environment variables, MCP
  configuration, runtimes, or visibility — not even as a suggestion inside a
  larger change.
- Creating, merging, retiring, or re-routing agents and squads is
  proposal-only: file a team or squad proposal describing the change for a
  person to carry out.
- Never paste chat messages; summarize them and point to the chat session.
  From comments, quote at most one excerpt of up to 120 characters. Never name
  members; say "a member" or describe their role. Remove anything resembling a
  secret.
- File proposals only with `multica evolution propose`; never create proposal
  issues or comment on them yourself. Mention every proposal in your run's
  report.
