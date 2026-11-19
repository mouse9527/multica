// Added by the mouse9527/multica fork: Evolution P2, the scheduled analysis
// settings (M-136).
//
// Shown under the enabled Evolution agent. Owners and admins edit the
// configuration; every member can read it, so members can see what Evolution
// reads. Turning chat reading on first asks for confirmation, because saving
// it notifies every member.

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { toast } from "sonner";
import {
  evolutionConfigOptions,
  startsChatReading,
  useSaveEvolutionConfig,
  type EvolutionConfig,
  type EvolutionConfigState,
} from "@multica/core/evolution";
import { useWorkspacePaths } from "@multica/core/paths";
import { projectListOptions } from "@multica/core/projects";
import { agentListOptions } from "@multica/core/workspace/queries";
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
import { Checkbox } from "@multica/ui/components/ui/checkbox";
import { Input } from "@multica/ui/components/ui/input";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import { Switch } from "@multica/ui/components/ui/switch";
import { Loader2 } from "@multica/ui/icons";
import { AppLink } from "../navigation";
import { useT } from "../i18n";

type NumberField = "max_items_per_window" | "max_window_days" | "max_runs_per_day";

export function EvolutionConfigSection({ wsId, agentId, canEdit }: { wsId: string; agentId: string; canEdit: boolean }) {
  const { t } = useT("evolution");
  const state = useQuery(evolutionConfigOptions(wsId));
  const save = useSaveEvolutionConfig(wsId);
  const [draft, setDraft] = useState<EvolutionConfig | null>(null);
  const [confirming, setConfirming] = useState(false);

  if (state.isLoading) return <Skeleton className="h-40 w-full" />;
  if (!state.data) return null;
  const current: EvolutionConfigState = state.data;
  const config = draft ?? current.config;
  const disabled = !canEdit || save.isPending;
  const update = (patch: Partial<EvolutionConfig>) => setDraft({ ...config, ...patch });

  const submit = async (acknowledgeMemberNotice: boolean) => {
    try {
      await save.mutateAsync({ config, acknowledgeMemberNotice });
      setDraft(null);
      setConfirming(false);
      toast.success(t(($) => $.config_saved));
    } catch (error) {
      toast.error(error instanceof Error ? error.message : t(($) => $.config_failed));
    }
  };
  const handleSave = () => {
    if (startsChatReading(current, config)) {
      setConfirming(true);
      return;
    }
    void submit(false);
  };

  return (
    <div className="flex flex-col gap-4 rounded-lg border p-4">
      <div className="flex items-center justify-between gap-4">
        <div className="min-w-0">
          <p className="text-body font-medium">{t(($) => $.config_title)}</p>
          <p className="text-caption text-muted-foreground">{t(($) => $.config_scope)}</p>
        </div>
        <Switch
          aria-label={t(($) => $.config_title)}
          checked={config.enabled}
          disabled={disabled}
          onCheckedChange={(checked) => update({ enabled: checked === true })}
        />
      </div>

      <ToggleRow
        label={t(($) => $.comments_label)}
        checked={config.sources.comments}
        disabled={disabled}
        onChange={(comments) => update({ sources: { ...config.sources, comments } })}
      />
      <ToggleRow
        label={t(($) => $.chat_label)}
        hint={
          config.sources.chat
            ? current.chat_active
              ? t(($) => $.chat_active)
              : t(($) => $.chat_pending)
            : undefined
        }
        checked={config.sources.chat}
        disabled={disabled}
        onChange={(chat) => update({ sources: { ...config.sources, chat } })}
      />

      <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
        <Field label={t(($) => $.schedule_time)}>
          <Input
            type="time"
            value={config.schedule.time}
            disabled={disabled}
            onChange={(e) => update({ schedule: { ...config.schedule, time: e.target.value } })}
          />
        </Field>
        <Field label={t(($) => $.schedule_timezone)}>
          <Input
            value={config.schedule.timezone}
            disabled={disabled}
            onChange={(e) => update({ schedule: { ...config.schedule, timezone: e.target.value } })}
          />
        </Field>
        <Field label={t(($) => $.schedule_interval)}>
          <NumberInput
            value={config.schedule.interval_days}
            range={current.limits.interval_days}
            disabled={disabled}
            onChange={(interval_days) => update({ schedule: { ...config.schedule, interval_days } })}
          />
        </Field>
        {(
          [
            ["max_items_per_window", t(($) => $.max_items), current.limits.items_per_window],
            ["max_window_days", t(($) => $.max_window_days), current.limits.window_days],
            ["max_runs_per_day", t(($) => $.max_runs), current.limits.runs_per_day],
          ] as [NumberField, string, [number, number]][]
        ).map(([field, label, range]) => (
          <Field key={field} label={label}>
            <NumberInput
              value={config[field]}
              range={range}
              disabled={disabled}
              onChange={(value) => setDraft({ ...config, [field]: value })}
            />
          </Field>
        ))}
      </div>

      <Exclusions wsId={wsId} selfAgentId={agentId} config={config} disabled={disabled} onChange={update} />

      {canEdit && (
        <div>
          <Button onClick={handleSave} disabled={draft === null || save.isPending}>
            {save.isPending && <Loader2 aria-hidden className="size-4 animate-spin" />}
            {t(($) => $.config_save)}
          </Button>
        </div>
      )}

      <RecentRuns runs={current.runs} />

      <AlertDialog open={confirming} onOpenChange={(open) => !save.isPending && setConfirming(open)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t(($) => $.notice_title)}</AlertDialogTitle>
            <AlertDialogDescription>{t(($) => $.notice_body)}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t(($) => $.notice_cancel)}</AlertDialogCancel>
            <Button onClick={() => void submit(true)} disabled={save.isPending}>
              {save.isPending && <Loader2 aria-hidden className="size-4 animate-spin" />}
              {t(($) => $.notice_confirm)}
            </Button>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function ToggleRow({
  label,
  hint,
  checked,
  disabled,
  onChange,
}: {
  label: string;
  hint?: string;
  checked: boolean;
  disabled: boolean;
  onChange: (checked: boolean) => void;
}) {
  return (
    <div className="flex items-center justify-between gap-4">
      <div className="min-w-0">
        <p className="text-body">{label}</p>
        {hint && <p className="text-caption text-muted-foreground">{hint}</p>}
      </div>
      <Switch aria-label={label} checked={checked} disabled={disabled} onCheckedChange={(v) => onChange(v === true)} />
    </div>
  );
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <label className="flex flex-col gap-1 text-caption text-muted-foreground">
      {label}
      {children}
    </label>
  );
}

function NumberInput({
  value,
  range,
  disabled,
  onChange,
}: {
  value: number;
  range: [number, number];
  disabled: boolean;
  onChange: (value: number) => void;
}) {
  return (
    <Input
      type="number"
      min={range[0]}
      max={range[1]}
      value={Number.isFinite(value) ? value : ""}
      disabled={disabled}
      onChange={(e) => onChange(e.target.valueAsNumber)}
    />
  );
}

function Exclusions({
  wsId,
  selfAgentId,
  config,
  disabled,
  onChange,
}: {
  wsId: string;
  selfAgentId: string;
  config: EvolutionConfig;
  disabled: boolean;
  onChange: (patch: Partial<EvolutionConfig>) => void;
}) {
  const { t } = useT("evolution");
  const projects = useQuery(projectListOptions(wsId));
  const agents = useQuery(agentListOptions(wsId));
  const projectItems = (projects.data ?? [])
    .filter((p) => !p.archived_at)
    .map((p) => ({ id: p.id, name: p.title }));
  const agentItems = (agents.data ?? [])
    .filter((a) => !a.archived_at && a.id !== selfAgentId)
    .map((a) => ({ id: a.id, name: a.name }));

  return (
    <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
      <CheckList
        label={t(($) => $.exclude_projects)}
        items={projectItems}
        selected={config.exclude_projects}
        disabled={disabled}
        onChange={(exclude_projects) => onChange({ exclude_projects })}
      />
      <CheckList
        label={t(($) => $.exclude_agents)}
        items={agentItems}
        selected={config.exclude_agents}
        disabled={disabled}
        onChange={(exclude_agents) => onChange({ exclude_agents })}
      />
    </div>
  );
}

function CheckList({
  label,
  items,
  selected,
  disabled,
  onChange,
}: {
  label: string;
  items: { id: string; name: string }[];
  selected: string[];
  disabled: boolean;
  onChange: (ids: string[]) => void;
}) {
  return (
    <fieldset className="flex min-w-0 flex-col gap-1">
      <legend className="text-caption text-muted-foreground">{label}</legend>
      <div className="flex max-h-40 flex-col gap-1 overflow-y-auto rounded-md border p-2">
        {items.map((item) => (
          <label key={item.id} className="flex min-w-0 items-center gap-2 text-body">
            <Checkbox
              checked={selected.includes(item.id)}
              disabled={disabled}
              onCheckedChange={(checked) =>
                onChange(checked === true ? [...selected, item.id] : selected.filter((id) => id !== item.id))
              }
            />
            <span className="truncate">{item.name}</span>
          </label>
        ))}
      </div>
    </fieldset>
  );
}

function RecentRuns({ runs }: { runs: EvolutionConfigState["runs"] }) {
  const { t } = useT("evolution");
  const paths = useWorkspacePaths();
  if (runs.length === 0) return null;
  const statusLabel = (status: string) =>
    status === "open"
      ? t(($) => $.run_open)
      : status === "succeeded"
        ? t(($) => $.run_succeeded)
        : status === "failed"
          ? t(($) => $.run_failed)
          : status;
  return (
    <div className="flex flex-col gap-1">
      <p className="text-caption text-muted-foreground">{t(($) => $.runs_title)}</p>
      <ul className="flex flex-col gap-1 text-body">
        {runs.map((run) => {
          const label = `${new Date(run.created_at).toLocaleString()} · ${statusLabel(run.status)}${
            run.kind === "catch_up" ? ` · ${t(($) => $.run_catch_up)}` : ""
          }`;
          return (
            <li key={run.id} className="flex min-w-0 items-center justify-between gap-2">
              {run.issue_id ? (
                <AppLink href={paths.issueDetail(run.issue_id)} className="truncate text-primary hover:underline">
                  {label}
                </AppLink>
              ) : (
                <span className="truncate">{label}</span>
              )}
              {run.failure_reason && (
                <span className="truncate text-caption text-muted-foreground" title={run.failure_reason}>
                  {run.failure_reason}
                </span>
              )}
            </li>
          );
        })}
      </ul>
    </div>
  );
}
