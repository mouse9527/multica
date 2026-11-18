// Added by the mouse9527/multica fork: the built-in Evolution agent (M-135).

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
