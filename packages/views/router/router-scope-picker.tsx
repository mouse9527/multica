// Added by the mouse9527/multica fork: Router page settings (M-105).

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { useWorkspaceId } from "@multica/core/hooks";
import { projectListOptions } from "@multica/core/projects/queries";
import { Button } from "@multica/ui/components/ui/button";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@multica/ui/components/ui/select";
import { ListTree, X } from "@multica/ui/icons";
import { IssuePickerModal } from "../modals/issue-picker-modal";
import { useT } from "../i18n";
import type { PendingScope } from "./router-form";
import { ScopeLabel } from "./router-labels";

/**
 * Adds projects and issue trees the member can see: the project list and
 * issue search already leave out private projects they are not in.
 * `taken` are scope ids that cannot be added again (already subscribed or
 * already chosen).
 */
export function RouterScopePicker({
  pending,
  onChange,
  taken,
  disabled,
}: {
  pending: PendingScope[];
  onChange: (next: PendingScope[]) => void;
  taken: string[];
  disabled?: boolean;
}) {
  const { t } = useT("router");
  const wsId = useWorkspaceId();
  const { data: projects = [] } = useQuery(projectListOptions(wsId));
  const [pickingIssue, setPickingIssue] = useState(false);
  const unavailable = new Set([...taken, ...pending.map((p) => p.scope_id)]);
  const projectItems = projects
    .filter((p) => !unavailable.has(p.id))
    .map((p) => ({ value: p.id, label: p.title }));

  return (
    <div className="space-y-2">
      {pending.length > 0 && (
        <ul className="space-y-1">
          {pending.map((scope) => (
            <li key={scope.scope_id} className="flex items-center gap-2 rounded-md border px-2 py-1.5 text-body">
              <span className="min-w-0 flex-1">
                <ScopeLabel scopeType={scope.scope_type} scopeId={scope.scope_id} />
              </span>
              <Button
                variant="ghost"
                size="icon-xs"
                aria-label={t(($) => $.scopes.remove)}
                disabled={disabled}
                onClick={() => onChange(pending.filter((p) => p.scope_id !== scope.scope_id))}
              >
                <X />
              </Button>
            </li>
          ))}
        </ul>
      )}
      <div className="flex flex-wrap gap-2">
        <Select
          items={projectItems}
          value={null}
          disabled={disabled || projectItems.length === 0}
          onValueChange={(id) => {
            const project = projects.find((p) => p.id === id);
            if (project) onChange([...pending, { scope_type: "project", scope_id: project.id, label: project.title }]);
          }}
        >
          <SelectTrigger size="sm" aria-label={t(($) => $.scopes.add_project)} className="min-w-40">
            <SelectValue placeholder={t(($) => $.scopes.add_project)} />
          </SelectTrigger>
          <SelectContent>
            {projectItems.map((item) => (
              <SelectItem key={item.value} value={item.value}>
                {item.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Button variant="outline" size="sm" disabled={disabled} onClick={() => setPickingIssue(true)}>
          <ListTree />
          {t(($) => $.scopes.add_issue_tree)}
        </Button>
      </div>
      <IssuePickerModal
        open={pickingIssue}
        onOpenChange={setPickingIssue}
        title={t(($) => $.scopes.picker_title)}
        description={t(($) => $.scopes.picker_description)}
        excludeIds={[...unavailable]}
        onSelect={(issue) =>
          onChange([...pending, { scope_type: "issue_tree", scope_id: issue.id, label: `${issue.identifier} ${issue.title}` }])
        }
      />
    </div>
  );
}
