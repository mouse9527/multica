// Added by the mouse9527/multica fork: Router page settings (M-105).

import { useQuery } from "@tanstack/react-query";
import { useWorkspaceId } from "@multica/core/hooks";
import { issueDetailOptions } from "@multica/core/issues/queries";
import { projectListOptions } from "@multica/core/projects/queries";
import { useWorkspacePaths } from "@multica/core/paths";
import {
  isSubscriptionPaused,
  routerStatus,
  type RouterSubscription,
} from "@multica/core/router";
import { Badge } from "@multica/ui/components/ui/badge";
import { FolderKanban, ListTree } from "@multica/ui/icons";
import { AppLink } from "../navigation";
import { useT } from "../i18n";

/** "M-12 Title" for an issue the member can see, "Unavailable" otherwise. */
export function IssueRefLabel({ issueId }: { issueId: string }) {
  const { t } = useT("router");
  const wsId = useWorkspaceId();
  const { data: issue, isError } = useQuery({ ...issueDetailOptions(wsId, issueId), retry: false });
  if (isError) return <span className="text-muted-foreground">{t(($) => $.scopes.unknown)}</span>;
  if (!issue) return <span className="text-muted-foreground">…</span>;
  return (
    <span className="min-w-0 truncate">
      <span className="text-muted-foreground">{issue.identifier}</span> {issue.title}
    </span>
  );
}

export function ProjectLabel({ projectId }: { projectId: string }) {
  const { t } = useT("router");
  const wsId = useWorkspaceId();
  const { data: projects } = useQuery(projectListOptions(wsId));
  const project = projects?.find((p) => p.id === projectId);
  return (
    <span className="min-w-0 truncate">
      {project?.title ?? <span className="text-muted-foreground">{t(($) => $.scopes.unknown)}</span>}
    </span>
  );
}

/** A subscription's scope with its kind icon. */
export function ScopeLabel({ scopeType, scopeId }: { scopeType: string; scopeId: string }) {
  const { t } = useT("router");
  const project = scopeType === "project";
  const Icon = project ? FolderKanban : ListTree;
  const kind = project ? t(($) => $.scopes.project) : t(($) => $.scopes.issue_tree);
  return (
    <span className="flex min-w-0 items-center gap-1.5">
      <Icon className="size-3.5 shrink-0 text-muted-foreground" aria-label={kind} />
      {project ? <ProjectLabel projectId={scopeId} /> : <IssueRefLabel issueId={scopeId} />}
    </span>
  );
}

/** Link to a project scope's board issue (its anchor). */
export function BoardLink({ issueId }: { issueId: string }) {
  const { t } = useT("router");
  const wsId = useWorkspaceId();
  const paths = useWorkspacePaths();
  const { data: issue } = useQuery({ ...issueDetailOptions(wsId, issueId), retry: false });
  if (!issue) return null;
  return (
    <AppLink href={paths.issueDetail(issue.id)} className="shrink-0 text-caption text-muted-foreground hover:text-foreground hover:underline">
      {t(($) => $.scopes.board, { identifier: issue.identifier })}
    </AppLink>
  );
}

export function usePauseReasonLabel() {
  const { t } = useT("router");
  return (reason: string) => {
    switch (reason) {
      case "rate":
        return t(($) => $.status.reason.rate);
      case "agent_unavailable":
        return t(($) => $.status.reason.agent_unavailable);
      default:
        return t(($) => $.status.reason.manual);
    }
  };
}

/** Running, or paused with every reason. */
export function RouterStatusBadge({ subs }: { subs: RouterSubscription[] }) {
  const { t } = useT("router");
  const reasonLabel = usePauseReasonLabel();
  const status = routerStatus(subs);
  if (status.kind === "running") {
    return <Badge variant="secondary">{t(($) => $.status.running)}</Badge>;
  }
  return (
    <span className="flex min-w-0 flex-wrap items-center gap-1.5">
      <Badge variant="destructive">{t(($) => $.status.paused)}</Badge>
      <span className="text-caption text-muted-foreground">{status.reasons.map(reasonLabel).join(" · ")}</span>
    </span>
  );
}

/** A per-scope pause marker; nothing while the scope runs. */
export function ScopePausedBadge({ sub }: { sub: RouterSubscription }) {
  const reasonLabel = usePauseReasonLabel();
  if (!isSubscriptionPaused(sub)) return null;
  const reason = sub.paused_reason ?? "manual";
  return (
    <Badge variant="outline" className="shrink-0">
      {reasonLabel(reason)}
    </Badge>
  );
}
