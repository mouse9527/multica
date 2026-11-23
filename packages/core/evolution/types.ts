// Added by the mouse9527/multica fork: the built-in Evolution agent (M-135, M-136, M-138).

import type { z } from "zod";
import type {
  EvolutionAgentSchema,
  EvolutionConfigSchema,
  EvolutionConfigStateSchema,
  EvolutionProposalStateSchema,
  EvolutionRunSchema,
  EvolutionVersionSchema,
} from "./schemas";

export type EvolutionAgent = z.infer<typeof EvolutionAgentSchema>;

/**
 * `available` is false when the endpoint answers 404 (`evolution_v1` off, or a
 * server without the feature).
 */
export type EvolutionStatus =
  | { available: false; agent: null }
  | { available: true; agent: EvolutionAgent | null };

export type EvolutionLanguage = "en" | "zh" | "ko" | "ja";

export interface EnableEvolutionInput {
  runtimeId: string;
  /** Empty falls back to the runtime's own default. */
  model?: string;
  language: EvolutionLanguage;
}

/** The scheduled analysis configuration (`workspace.settings.evolution`). */
export type EvolutionConfig = z.infer<typeof EvolutionConfigSchema>;

/** One analysis run. */
export type EvolutionRun = z.infer<typeof EvolutionRunSchema>;

/**
 * The configuration, what is actually being read (`chat_active` is true only
 * after members were notified), the server's bounds, and the latest runs.
 */
export type EvolutionConfigState = z.infer<typeof EvolutionConfigStateSchema>;

export interface SaveEvolutionConfigInput {
  config: EvolutionConfig;
  /** Confirms that turning chat reading on notifies every member. */
  acknowledgeMemberNotice?: boolean;
}

/** A proposal's apply and rollback state (`GET /api/evolution/proposals/{issueId}`). */
export type EvolutionProposalState = z.infer<typeof EvolutionProposalStateSchema>;

/** One apply or rollback of a proposal. */
export type EvolutionVersion = z.infer<typeof EvolutionVersionSchema>;
