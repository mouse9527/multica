// Added by the mouse9527/multica fork: the built-in Evolution agent (M-135).

import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { workspaceKeys } from "../workspace/queries";
import { enableEvolution, getEvolution } from "./api";
import type { EnableEvolutionInput } from "./types";

export const evolutionKeys = {
  all: (wsId: string) => ["evolution", wsId] as const,
  status: (wsId: string) => [...evolutionKeys.all(wsId), "status"] as const,
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
