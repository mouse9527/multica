// Added by the mouse9527/multica fork: the built-in Evolution agent (M-135, M-136).

import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { workspaceKeys } from "../workspace/queries";
import { enableEvolution, getEvolution, getEvolutionConfig, saveEvolutionConfig } from "./api";
import type { EnableEvolutionInput, SaveEvolutionConfigInput } from "./types";

export const evolutionKeys = {
  all: (wsId: string) => ["evolution", wsId] as const,
  status: (wsId: string) => [...evolutionKeys.all(wsId), "status"] as const,
  config: (wsId: string) => [...evolutionKeys.all(wsId), "config"] as const,
};

export function evolutionStatusOptions(wsId: string) {
  return queryOptions({
    queryKey: evolutionKeys.status(wsId),
    queryFn: () => getEvolution(wsId),
  });
}

export function useEnableEvolution(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: EnableEvolutionInput) => enableEvolution(wsId, input),
    onSettled: () =>
      Promise.all([
        qc.invalidateQueries({ queryKey: evolutionKeys.all(wsId) }),
        qc.invalidateQueries({ queryKey: workspaceKeys.agents(wsId) }),
      ]),
  });
}

export function evolutionConfigOptions(wsId: string) {
  return queryOptions({
    queryKey: evolutionKeys.config(wsId),
    queryFn: () => getEvolutionConfig(wsId),
  });
}

export function useSaveEvolutionConfig(wsId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: SaveEvolutionConfigInput) => saveEvolutionConfig(wsId, input),
    onSuccess: (state) => qc.setQueryData(evolutionKeys.config(wsId), state),
    onSettled: () => qc.invalidateQueries({ queryKey: evolutionKeys.config(wsId) }),
  });
}
