// Added by the mouse9527/multica fork: Router page settings (M-105).

import { ROUTER_EVENT_TYPES, type RouterEventType, type RouterSettingsInput } from "@multica/core/router";
import { Checkbox } from "@multica/ui/components/ui/checkbox";
import { Input } from "@multica/ui/components/ui/input";
import { Label } from "@multica/ui/components/ui/label";
import { useT } from "../i18n";
import { validBatching } from "./router-form";

export function RouterSettingsFields({
  value,
  onChange,
  disabled,
}: {
  value: RouterSettingsInput;
  onChange: (next: RouterSettingsInput) => void;
  disabled?: boolean;
}) {
  const { t } = useT("router");
  const eventLabel = (type: RouterEventType) => {
    switch (type) {
      case "issue_created":
        return t(($) => $.events.issue_created);
      case "status_changed":
        return t(($) => $.events.status_changed);
      case "assignee_changed":
        return t(($) => $.events.assignee_changed);
      case "comment_created":
        return t(($) => $.events.comment_created);
      case "task_failed":
        return t(($) => $.events.task_failed);
    }
  };
  const toggle = (type: RouterEventType, on: boolean) => {
    const set = new Set(value.event_types);
    if (on) set.add(type);
    else set.delete(type);
    onChange({ ...value, event_types: ROUTER_EVENT_TYPES.filter((e) => set.has(e)) });
  };
  const number = (key: "debounce_seconds" | "max_wait_seconds" | "max_runs_per_hour", label: string) => (
    <div className="space-y-1">
      <Label htmlFor={`router-${key}`} className="text-caption text-muted-foreground">
        {label}
      </Label>
      <Input
        id={`router-${key}`}
        type="number"
        inputMode="numeric"
        disabled={disabled}
        value={Number.isNaN(value[key]) ? "" : value[key]}
        onChange={(e) => onChange({ ...value, [key]: e.target.value === "" ? Number.NaN : Number(e.target.value) })}
      />
    </div>
  );

  return (
    <div className="space-y-4">
      <fieldset className="space-y-2">
        <legend className="mb-2 text-body font-medium">{t(($) => $.events.title)}</legend>
        <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
          {ROUTER_EVENT_TYPES.map((type) => (
            <label key={type} className="flex items-center gap-2 text-body">
              <Checkbox
                checked={value.event_types.includes(type)}
                disabled={disabled}
                onCheckedChange={(on) => toggle(type, on === true)}
              />
              {eventLabel(type)}
            </label>
          ))}
        </div>
      </fieldset>
      <fieldset className="space-y-2">
        <legend className="mb-2 text-body font-medium">{t(($) => $.batching.title)}</legend>
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
          {number("debounce_seconds", t(($) => $.batching.debounce))}
          {number("max_wait_seconds", t(($) => $.batching.max_wait))}
          {number("max_runs_per_hour", t(($) => $.batching.max_runs))}
        </div>
        {!validBatching(value) && (
          <p role="alert" className="text-caption text-destructive">
            {t(($) => $.batching.invalid)}
          </p>
        )}
      </fieldset>
    </div>
  );
}
