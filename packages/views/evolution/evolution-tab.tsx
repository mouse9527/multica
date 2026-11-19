// Added by the mouse9527/multica fork: the built-in Evolution agent (M-135, M-136).
//
// Settings → Evolution: a workspace owner or admin enables the built-in
// Evolution agent by picking the runtime it runs on. Once enabled, the page
// links to the agent and shows the scheduled analysis settings (M-136).

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { toast } from "sonner";
import { useAuthStore } from "@multica/core/auth";
import { useWorkspaceId } from "@multica/core/hooks";
import { useCurrentMember } from "@multica/core/permissions";
import { useWorkspacePaths } from "@multica/core/paths";
import { runtimeListOptions } from "@multica/core/runtimes";
import { evolutionStatusOptions, useEnableEvolution } from "@multica/core/evolution";
import { Button } from "@multica/ui/components/ui/button";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import { Loader2 } from "@multica/ui/icons";
import { AppLink } from "../navigation";
import { EvolutionConfigSection } from "./evolution-config";
import { useT } from "../i18n";
import { pickContentLang } from "../onboarding/templates";
import { SettingsTab } from "../settings/components/settings-layout";
import { MikaRuntimeChoice, type MikaRuntimeSelection } from "../runtimes/components/mika-runtime-choice";

export function EvolutionTab() {
  const { t, i18n } = useT("evolution");
  const wsId = useWorkspaceId();
  const paths = useWorkspacePaths();
  const currentUserId = useAuthStore((s) => s.user?.id) ?? null;
  const { role } = useCurrentMember(wsId);
  const canEnable = role === "owner" || role === "admin";
  const status = useQuery(evolutionStatusOptions(wsId));
  const runtimes = useQuery({ ...runtimeListOptions(wsId), enabled: canEnable });
  const enable = useEnableEvolution(wsId);
  const [choice, setChoice] = useState<MikaRuntimeSelection>({ runtimeId: "", model: "" });

  const agent = status.data?.agent ?? null;

  const handleEnable = async () => {
    if (!choice.runtimeId || enable.isPending) return;
    try {
      await enable.mutateAsync({
        runtimeId: choice.runtimeId,
        model: choice.model || undefined,
        language: pickContentLang(i18n.language),
      });
      toast.success(t(($) => $.enabled_toast));
    } catch (error) {
      toast.error(error instanceof Error ? error.message : t(($) => $.failed));
    }
  };

  return (
    <SettingsTab title={t(($) => $.tab)}>
      <p className="text-body text-muted-foreground">{t(($) => $.description)}</p>
      {status.isLoading ? (
        <Skeleton className="h-16 w-full" />
      ) : agent ? (
        <>
          <div className="flex items-center justify-between gap-4 rounded-lg border p-4">
            <div className="min-w-0">
              <p className="text-body font-medium">{t(($) => $.enabled_title)}</p>
              <p className="truncate text-caption text-muted-foreground">{agent.name}</p>
            </div>
            <AppLink href={paths.agentDetail(agent.id)} className="shrink-0 text-body text-primary hover:underline">
              {t(($) => $.open_agent)}
            </AppLink>
          </div>
          <EvolutionConfigSection wsId={wsId} agentId={agent.id} canEdit={canEnable} />
        </>
      ) : canEnable ? (
        <div className="flex flex-col gap-4 rounded-lg border p-4">
          <p className="text-body">{t(($) => $.enable_hint)}</p>
          <MikaRuntimeChoice
            runtimes={runtimes.data ?? []}
            runtimesLoading={runtimes.isLoading}
            currentUserId={currentUserId}
            value={choice}
            onChange={setChoice}
            disabled={enable.isPending}
          />
          <div>
            <Button onClick={handleEnable} disabled={!choice.runtimeId || enable.isPending}>
              {enable.isPending && <Loader2 aria-hidden className="size-4 animate-spin" />}
              {t(($) => $.enable)}
            </Button>
          </div>
        </div>
      ) : (
        <p className="text-body text-muted-foreground">{t(($) => $.not_enabled_member)}</p>
      )}
    </SettingsTab>
  );
}
