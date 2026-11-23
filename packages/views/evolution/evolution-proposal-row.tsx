// Added by the mouse9527/multica fork: apply and roll back an Evolution proposal (M-138).

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { toast } from "sonner";
import { useFeatureEnabled } from "@multica/core/config";
import { useWorkspaceId } from "@multica/core/hooks";
import {
  EVOLUTION_FLAG,
  evolutionProposalOptions,
  useChangeEvolutionProposal,
  type EvolutionProposalState,
} from "@multica/core/evolution";
import {
  AlertDialog,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@multica/ui/components/ui/alert-dialog";
import { Button } from "@multica/ui/components/ui/button";
import { Loader2 } from "@multica/ui/icons";
import { PropRow } from "../common/prop-row";
import { useT } from "../i18n";

type Action = "apply" | "rollback";

/**
 * The issue sidebar's Evolution row on a proposal issue: where the proposal
 * stands and, for workspace owners and admins, Apply or Roll back. Only
 * issues Evolution filed (they carry evolution_fingerprint) are asked about;
 * nothing while `evolution_v1` is off.
 */
export function EvolutionProposalRow({
  issueId,
  metadata,
}: {
  issueId: string;
  metadata?: Record<string, unknown> | null;
}) {
  const wsId = useWorkspaceId();
  const enabled = useFeatureEnabled(EVOLUTION_FLAG, false);
  const isProposal = typeof metadata?.evolution_fingerprint === "string";
  const { data: proposal } = useQuery({
    ...evolutionProposalOptions(wsId, issueId),
    enabled: enabled && isProposal && Boolean(issueId),
    retry: false,
  });
  if (!enabled || !isProposal || !proposal) return null;
  return <ProposalRow wsId={wsId} issueId={issueId} proposal={proposal} />;
}

function ProposalRow({ wsId, issueId, proposal }: { wsId: string; issueId: string; proposal: EvolutionProposalState }) {
  const { t } = useT("evolution");
  const change = useChangeEvolutionProposal(wsId, issueId);
  const [confirming, setConfirming] = useState<Action | null>(null);
  const lastApply = [...proposal.versions].reverse().find((v) => v.action === "apply");
  const fieldLabel = useFieldLabel();
  const target =
    proposal.target_type === "workflow"
      ? t(($) => $.proposal.target_workflow, { name: proposal.target_name })
      : t(($) => $.proposal.target_agent, { name: proposal.target_name });
  const field = fieldLabel(proposal.field);

  let status: string;
  if (!proposal.applicable) status = t(($) => $.proposal.by_hand);
  else if (proposal.state === "applied")
    status = proposal.changed_since_apply
      ? t(($) => $.proposal.changed_since_apply, { version: lastApply?.version ?? 0 })
      : t(($) => $.proposal.applied, { version: lastApply?.version ?? 0 });
  else if (proposal.stale) status = t(($) => $.proposal.stale);
  else if (proposal.state === "rolled_back") status = t(($) => $.proposal.rolled_back);
  else status = t(($) => $.proposal.pending);

  const allowed = (action: Action) => (action === "apply" ? proposal.can_apply : proposal.can_rollback);

  async function run(action: Action) {
    try {
      await change.mutateAsync(action);
      setConfirming(null);
      toast.success(action === "apply" ? t(($) => $.proposal.applied_toast) : t(($) => $.proposal.rolled_back_toast));
    } catch (error) {
      toast.error(error instanceof Error && error.message ? error.message : t(($) => $.proposal.failed));
    }
  }

  return (
    <PropRow interactive={false} label={t(($) => $.proposal.label)}>
      <div className="flex min-w-0 flex-1 items-center justify-between gap-2">
        <span className="truncate text-muted-foreground">{status}</span>
        {proposal.can_apply && (
          <Button size="xs" variant="outline" onClick={() => setConfirming("apply")}>
            {t(($) => $.proposal.apply)}
          </Button>
        )}
        {proposal.can_rollback && (
          <Button size="xs" variant="outline" onClick={() => setConfirming("rollback")}>
            {t(($) => $.proposal.rollback)}
          </Button>
        )}
      </div>

      <AlertDialog open={confirming !== null} onOpenChange={(open) => !change.isPending && !open && setConfirming(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {confirming === "rollback" ? t(($) => $.proposal.rollback_title) : t(($) => $.proposal.apply_title)}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {confirming === "rollback"
                ? t(($) => $.proposal.rollback_body, { field, target, version: lastApply?.version ?? 0 })
                : t(($) => $.proposal.apply_body, { field, target })}
            </AlertDialogDescription>
          </AlertDialogHeader>
          {confirming === "apply" && (
            <div className="flex min-w-0 flex-col gap-2">
              {proposal.change.before !== "" && (
                <ChangeSide label={t(($) => $.proposal.before)} text={proposal.change.before} />
              )}
              <ChangeSide label={t(($) => $.proposal.after)} text={proposal.change.after} />
            </div>
          )}
          <AlertDialogFooter>
            <AlertDialogCancel>{t(($) => $.proposal.cancel)}</AlertDialogCancel>
            {confirming && (
              <Button
                onClick={() => void run(confirming)}
                disabled={change.isPending || !allowed(confirming)}
                aria-busy={change.isPending}
              >
                {change.isPending && <Loader2 aria-hidden className="size-4 animate-spin" />}
                {confirming === "rollback" ? t(($) => $.proposal.rollback) : t(($) => $.proposal.apply)}
              </Button>
            )}
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </PropRow>
  );
}

function ChangeSide({ label, text }: { label: string; text: string }) {
  return (
    <div className="flex min-w-0 flex-col gap-1">
      <span className="text-caption text-muted-foreground">{label}</span>
      <pre className="max-h-48 overflow-auto whitespace-pre-wrap break-words rounded-md bg-muted p-2 text-caption">{text}</pre>
    </div>
  );
}

function useFieldLabel() {
  const { t } = useT("evolution");
  return (field: string) => {
    switch (field) {
      case "instructions":
        return t(($) => $.proposal.fields.instructions);
      case "description":
        return t(($) => $.proposal.fields.description);
      case "skills":
        return t(($) => $.proposal.fields.skills);
      case "steps":
        return t(($) => $.proposal.fields.steps);
      case "name":
        return t(($) => $.proposal.fields.name);
      default:
        return field;
    }
  };
}
