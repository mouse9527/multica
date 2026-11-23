// Added by the mouse9527/multica fork: the overlap refusal on subscribe (M-194).

import { RouterScopeOverlapError, type AddRouterScopesResult, type RouterOverlap } from "@multica/core/router";
import { useT } from "../i18n";

/** The scopes refused because other Super Agents already receive their events, by scope_id. */
export function overlapsOf(result: AddRouterScopesResult): Map<string, RouterOverlap[]> {
  const out = new Map<string, RouterOverlap[]>();
  for (const f of result.failed) {
    if (f.error instanceof RouterScopeOverlapError) out.set(f.scope.scope_id, f.error.overlaps);
  }
  return out;
}

/**
 * Names the Super Agents a pending scope overlaps. The form keeps those
 * scopes; submitting again adds them anyway.
 */
export function RouterOverlapWarning({
  overlaps,
  labels,
  action,
}: {
  overlaps: Map<string, RouterOverlap[]>;
  labels: Map<string, string>;
  action: string;
}) {
  const { t } = useT("router");
  const shown = [...overlaps].filter(([scopeId]) => labels.has(scopeId));
  if (shown.length === 0) return null;
  return (
    <div role="alert" className="space-y-1.5 rounded-md border border-warning/40 bg-warning/10 px-3 py-2 text-body">
      <p className="font-medium">{t(($) => $.overlap.title)}</p>
      <ul className="space-y-1">
        {shown.flatMap(([scopeId, list]) =>
          list.map((o) => (
            <li key={`${scopeId}:${o.subscription_id}`} className="min-w-0">
              {t(($) => $.overlap.item, {
                scope: labels.get(scopeId) ?? scopeId,
                agent: o.agent_name || o.agent_id,
                other: o.scope_name || t(($) => $.scopes.unknown),
                n: o.issue_count,
              })}
              {o.issues.length > 0 && (
                <span className="text-muted-foreground"> {t(($) => $.overlap.examples, { issues: o.issues.join(", ") })}</span>
              )}
            </li>
          )),
        )}
      </ul>
      <p className="text-caption text-muted-foreground">{t(($) => $.overlap.hint, { action })}</p>
    </div>
  );
}
