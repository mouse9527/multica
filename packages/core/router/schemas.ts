// Added by the mouse9527/multica fork: Router page settings (M-105), the
// chat list's Router marker (M-108), and issue coverage and overlaps (M-194).

import { z } from "zod";

const nullableString = z.string().nullable().optional().transform((v) => v ?? null);

export const RouterSubscriptionSchema = z.object({
  id: z.string(),
  workspace_id: z.string().default(""),
  agent_id: z.string(),
  owner_user_id: nullableString,
  scope_type: z.string(),
  scope_id: z.string(),
  anchor_issue_id: nullableString,
  anchor_chat_session_id: nullableString,
  event_types: z.array(z.string()).nullable().optional().transform((v) => v ?? []),
  enabled: z.boolean().default(true),
  paused_reason: nullableString,
  debounce_seconds: z.number().default(60),
  max_wait_seconds: z.number().default(600),
  max_runs_per_hour: z.number().default(20),
  reconciled_through: nullableString,
  created_by_type: z.string().default(""),
  created_by_id: z.string().default(""),
  created_at: z.string().default(""),
  event_count: z.number().default(0),
  capture_miss: z.number().default(0),
});

export const RouterSubscriptionListSchema = z.object({
  subscriptions: z.array(RouterSubscriptionSchema).nullable().transform((v) => v ?? []),
});

export const RouterDispatchSchema = z.object({
  id: z.string(),
  subscription_id: z.string(),
  agent_id: z.string().default(""),
  anchor_issue_id: z.string().default(""),
  task_id: z.string().default(""),
  task_status: nullableString,
  event_count: z.number().default(0),
  outcome: nullableString,
  settled_at: nullableString,
  created_at: z.string().default(""),
});

export const RouterDispatchListSchema = z.object({
  dispatches: z.array(RouterDispatchSchema).nullable().transform((v) => v ?? []),
});

export const RouterSubscriptionChangeListSchema = z.object({
  changes: z
    .array(
      z.object({
        id: z.string(),
        subscription_id: z.string().default(""),
        action: z.string(),
        actor_type: z.string().default(""),
        detail: z.record(z.string(), z.unknown()).nullable().optional().transform((v) => v ?? {}),
        created_at: z.string().default(""),
      }),
    )
    .nullable()
    .transform((v) => v ?? []),
});

const stringList = z.array(z.string()).nullable().optional().transform((v) => v ?? []);

export const RouterChatSummaryListSchema = z.object({
  chats: z
    .array(
      z.object({
        chat_session_id: z.string(),
        paused_reasons: stringList,
        scopes: z
          .array(
            z.object({
              subscription_id: z.string(),
              scope_type: z.string(),
              scope_id: z.string(),
              name: z.string().default(""),
              identifier: z.string().default(""),
              visible: z.boolean().default(false),
              paused_reason: nullableString,
            }),
          )
          .nullable()
          .optional()
          .transform((v) => v ?? []),
      }),
    )
    .nullable()
    .transform((v) => v ?? []),
});

export const RouterCoveringListSchema = z.object({
  covering: z
    .array(
      z.object({
        subscription_id: z.string(),
        agent_id: z.string().default(""),
        agent_name: z.string().default(""),
        owner_user_id: nullableString,
        scope_type: z.string().default(""),
        scope_id: z.string().default(""),
        scope_name: z.string().default(""),
        chat_bound: z.boolean().default(false),
        chat_session_id: nullableString,
        chat_title: nullableString,
        enabled: z.boolean().default(true),
        paused_reason: nullableString,
        effective: z.boolean().default(false),
        superseded_by: nullableString,
      }),
    )
    .nullable()
    .transform((v) => v ?? []),
});

export const RouterOverlapRefusalSchema = z.object({
  code: z.literal("router_scope_overlap"),
  overlaps: z
    .array(
      z.object({
        subscription_id: z.string(),
        agent_id: z.string().default(""),
        agent_name: z.string().default(""),
        scope_type: z.string().default(""),
        scope_id: z.string().default(""),
        scope_name: z.string().default(""),
        issue_count: z.number().default(0),
        issues: stringList,
      }),
    )
    .nullable()
    .optional()
    .transform((v) => v ?? []),
});
