// Added by the mouse9527/multica fork: Router page settings (M-105) and the
// chat list's Router marker (M-108).
//
// A Router is a chat session plus the subscriptions bound to it
// (`anchor_chat_session_id`); its agent is the chat's agent. The server only
// knows subscriptions, so the client groups them by chat.

export type RouterScopeType = "issue_tree" | "project";

export const ROUTER_EVENT_TYPES = [
  "issue_created",
  "status_changed",
  "assignee_changed",
  "comment_created",
  "task_failed",
] as const;

export type RouterEventType = (typeof ROUTER_EVENT_TYPES)[number];

/** Server defaults and limits (server/internal/agentrouter/agentrouter.go). */
export const ROUTER_BATCHING = {
  defaultDebounceSeconds: 60,
  defaultMaxWaitSeconds: 600,
  defaultMaxRunsPerHour: 20,
  minSeconds: 10,
  maxSeconds: 86400,
  maxRunsPerHourLimit: 60,
} as const;

export interface RouterSubscription {
  id: string;
  workspace_id: string;
  agent_id: string;
  owner_user_id: string | null;
  scope_type: string;
  scope_id: string;
  anchor_issue_id: string | null;
  anchor_chat_session_id: string | null;
  event_types: string[];
  /** False: paused by hand, capture stopped. */
  enabled: boolean;
  /** "manual" with enabled false; "rate" or "agent_unavailable" with enabled
   *  true (dispatch stopped, capture goes on). */
  paused_reason: string | null;
  debounce_seconds: number;
  max_wait_seconds: number;
  max_runs_per_hour: number;
  reconciled_through: string | null;
  created_by_type: string;
  created_by_id: string;
  created_at: string;
  event_count: number;
  capture_miss: number;
}

export interface RouterDispatch {
  id: string;
  subscription_id: string;
  agent_id: string;
  anchor_issue_id: string;
  task_id: string;
  task_status: string | null;
  event_count: number;
  outcome: string | null;
  settled_at: string | null;
  created_at: string;
}

export interface RouterSubscriptionChange {
  id: string;
  subscription_id: string;
  action: string;
  actor_type: string;
  detail: Record<string, unknown>;
  created_at: string;
}

export interface RouterSettingsInput {
  event_types: string[];
  debounce_seconds: number;
  max_wait_seconds: number;
  max_runs_per_hour: number;
}

export interface RouterScopeInput {
  scope_type: RouterScopeType;
  /** Issue id (issue_tree) or project id. */
  scope_id: string;
}

/** One Router: a chat and the subscriptions bound to it. */
export interface RouterInstance {
  chat_session_id: string;
  agent_id: string;
  subscriptions: RouterSubscription[];
}

export type RouterStatus =
  | { kind: "running" }
  | { kind: "paused"; reasons: string[] };

/** One scope of a Router as the chat list shows it (`GET /api/router/chats`). */
export interface RouterChatScope {
  subscription_id: string;
  scope_type: string;
  scope_id: string;
  /** Project title or root issue title; empty when not `visible`. */
  name: string;
  /** Root issue identifier of an issue tree; empty for a project. */
  identifier: string;
  /** False when the scope is gone or in a project the member cannot see. */
  visible: boolean;
  /** "manual", "rate" or "agent_unavailable"; null while it runs. */
  paused_reason: string | null;
}

/** A Router among the member's chats, for the chat list and header marker. */
export interface RouterChatSummary {
  chat_session_id: string;
  /** Distinct reasons of its paused scopes; empty while all run. */
  paused_reasons: string[];
  scopes: RouterChatScope[];
}
