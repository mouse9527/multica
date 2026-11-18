// Added by the mouse9527/multica fork: the built-in Evolution agent (M-135).

import type { z } from "zod";
import type { EvolutionAgentSchema } from "./schemas";

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
