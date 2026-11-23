// Added by the mouse9527/multica fork: the built-in Evolution agent (M-135, M-136, M-137, M-138).

import { z } from "zod";

const nullableString = z.string().nullable().optional().transform((v) => v ?? null);

/** The fields of the agent the Evolution settings page shows. */
export const EvolutionAgentSchema = z.object({
  id: z.string(),
  name: z.string().default(""),
  avatar_url: nullableString,
  runtime_id: nullableString,
  model: nullableString,
  status: z.string().default(""),
});

export const EvolutionStatusSchema = z.object({
  agent: EvolutionAgentSchema.nullable().optional().transform((v) => v ?? null),
});

// P2 (M-136): the scheduled analysis configuration. Defaults mirror the
// server's (internal/evolution/config.go) so an older server or a malformed
// body still renders a usable form.

const range = (lo: number, hi: number) => z.tuple([z.number(), z.number()]).catch([lo, hi]);

export const EvolutionConfigSchema = z.object({
  enabled: z.boolean().catch(false),
  schedule: z
    .object({
      time: z.string().catch("03:00"),
      timezone: z.string().catch("Asia/Shanghai"),
      interval_days: z.number().int().catch(1),
    })
    .catch({ time: "03:00", timezone: "Asia/Shanghai", interval_days: 1 }),
  sources: z
    .object({ comments: z.boolean().catch(true), chat: z.boolean().catch(false) })
    .catch({ comments: true, chat: false }),
  exclude_projects: z.array(z.string()).nullable().catch([]).transform((v) => v ?? []),
  exclude_agents: z.array(z.string()).nullable().catch([]).transform((v) => v ?? []),
  max_items_per_window: z.number().int().catch(2000),
  max_window_days: z.number().int().catch(7),
  max_runs_per_day: z.number().int().catch(3),
  // P3 (M-137): proposal bounds. Kept so a save round-trips them.
  max_proposals_per_run: z.number().int().catch(5),
  min_evidence_count: z.number().int().catch(2),
  rejected_cooldown_days: z.number().int().catch(30),
});

export const EvolutionRunSchema = z.object({
  id: z.string(),
  kind: z.string().catch(""),
  status: z.string().catch(""),
  issue_id: nullableString,
  window_until: z.string().catch(""),
  served_items: z.number().catch(0),
  failure_reason: z.string().catch(""),
  created_at: z.string().catch(""),
  finished_at: nullableString,
});

export const EvolutionLimitsSchema = z
  .object({
    items_per_window: range(50, 5000),
    window_days: range(1, 30),
    runs_per_day: range(1, 10),
    interval_days: range(1, 7),
    max_excluded: z.number().catch(200),
  })
  .catch({ items_per_window: [50, 5000], window_days: [1, 30], runs_per_day: [1, 10], interval_days: [1, 7], max_excluded: 200 });

export const EvolutionConfigStateSchema = z.object({
  config: EvolutionConfigSchema.catch(() => EvolutionConfigSchema.parse({})),
  chat_active: z.boolean().catch(false),
  comments_active: z.boolean().catch(false),
  limits: EvolutionLimitsSchema,
  runs: z.array(EvolutionRunSchema).catch([]),
});

// P4 (M-138): a proposal's apply and rollback state, for its issue page.

export const EvolutionVersionSchema = z.object({
  id: z.string(),
  version: z.number().catch(0),
  action: z.string().catch(""),
  field: z.string().catch(""),
  reverted_of: nullableString,
  actor_id: z.string().catch(""),
  created_at: z.string().catch(""),
});

export const EvolutionProposalStateSchema = z.object({
  issue_id: z.string(),
  target_type: z.string().catch(""),
  target_id: z.string().catch(""),
  target_name: z.string().catch(""),
  field: z.string().catch(""),
  change: z.object({ before: z.string().catch(""), after: z.string().catch("") }).catch({ before: "", after: "" }),
  /** pending, applied, or rolled_back; anything else reads as pending. */
  state: z.enum(["pending", "applied", "rolled_back"]).catch("pending"),
  applicable: z.boolean().catch(false),
  stale: z.boolean().catch(false),
  changed_since_apply: z.boolean().catch(false),
  // Affordances default to off: an unreadable answer offers nothing.
  can_apply: z.boolean().catch(false),
  can_rollback: z.boolean().catch(false),
  versions: z.array(EvolutionVersionSchema).catch([]),
});
