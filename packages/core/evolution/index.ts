// Added by the mouse9527/multica fork: the built-in Evolution agent (M-135).

export * from "./types";
export { enableEvolution, getEvolution } from "./api";
export { evolutionKeys, evolutionStatusOptions, useEnableEvolution } from "./queries";

/** Frontend flag: the server's evolution_v1. */
export const EVOLUTION_FLAG = "evolution_v1";
