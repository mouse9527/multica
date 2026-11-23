// Added by the mouse9527/multica fork: the built-in Evolution agent (M-135, M-136, M-138).

import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { issueKeys } from "../issues/queries";
import { workspaceKeys } from "../workspace/queries";
import {
  applyEvolutionProposal,
  enableEvolution,
  getEvolution,
  getEvolutionConfig,
  getEvolutionProposal,
  rollbackEvolutionProposal,
  saveEvolutionConfig,
} from "./api";
import type { EnableEvolutionInput, SaveEvolutionConfigInput } from "./types";

export const evolutionKeys = {
  all: (wsId: string) => ["evolution", wsId] as const,
  status: (wsId: string) => [...evolutionKeys.all(wsId), "status"] as const,
  config: (wsId: string) => [...evolutionKeys.all(wsId), "config"] as const,
  proposal: (wsId: string, issueId: string) => [...evolutionKeys.all(wsId), "proposal", issueId] as const,
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

export function evolutionProposalOptions(wsId: string, issueId: string) {
  return queryOptions({
    queryKey: evolutionKeys.proposal(wsId, issueId),
    queryFn: () => getEvolutionProposal(wsId, issueId),
  });
}

/**
 * Apply or roll back a proposal. Either one changes an agent or workflow and
 * comments on the proposal issue, so those caches are refreshed too; after
 * a refusal the state is re-read (a stale apply marks the proposal stale).
 */
export function useChangeEvolutionProposal(wsId: string, issueId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (action: "apply" | "rollback") =>
      action === "apply" ? applyEvolutionProposal(wsId, issueId) : rollbackEvolutionProposal(wsId, issueId),
    onSuccess: (state) => qc.setQueryData(evolutionKeys.proposal(wsId, issueId), state),
    onSettled: () =>
      Promise.all([
        qc.invalidateQueries({ queryKey: evolutionKeys.proposal(wsId, issueId) }),
        qc.invalidateQueries({ queryKey: issueKeys.detail(wsId, issueId) }),
        qc.invalidateQueries({ queryKey: issueKeys.timeline(issueId) }),
        qc.invalidateQueries({ queryKey: workspaceKeys.agents(wsId) }),
      ]),
  });
}
