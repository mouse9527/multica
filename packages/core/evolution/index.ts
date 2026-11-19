// Added by the mouse9527/multica fork: the built-in Evolution agent (M-135, M-136).

export * from "./types";
export { enableEvolution, getEvolution, getEvolutionConfig, saveEvolutionConfig, startsChatReading } from "./api";
export {
  evolutionConfigOptions,
  evolutionKeys,
  evolutionStatusOptions,
  useEnableEvolution,
  useSaveEvolutionConfig,
} from "./queries";

/** Frontend flag: the server's evolution_v1. */
export const EVOLUTION_FLAG = "evolution_v1";
