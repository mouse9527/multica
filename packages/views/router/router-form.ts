// Added by the mouse9527/multica fork: Router page settings (M-105).

import {
  ROUTER_BATCHING,
  ROUTER_EVENT_TYPES,
  type RouterScopeInput,
  type RouterSettingsInput,
  type RouterSubscription,
} from "@multica/core/router";

/** A scope chosen in the form but not created yet, with its display label. */
export interface PendingScope extends RouterScopeInput {
  label: string;
}

export const DEFAULT_ROUTER_SETTINGS: RouterSettingsInput = {
  event_types: [...ROUTER_EVENT_TYPES],
  debounce_seconds: ROUTER_BATCHING.defaultDebounceSeconds,
  max_wait_seconds: ROUTER_BATCHING.defaultMaxWaitSeconds,
  max_runs_per_hour: ROUTER_BATCHING.defaultMaxRunsPerHour,
};

/**
 * The form starts from the Router's first subscription; the settings page
 * writes the same values to every subscription of the Router.
 */
export function settingsOf(subs: RouterSubscription[]): RouterSettingsInput {
  const first = subs[0];
  if (!first) return DEFAULT_ROUTER_SETTINGS;
  return {
    event_types: first.event_types.length ? [...first.event_types] : [...ROUTER_EVENT_TYPES],
    debounce_seconds: first.debounce_seconds,
    max_wait_seconds: first.max_wait_seconds,
    max_runs_per_hour: first.max_runs_per_hour,
  };
}

/** The server's batching limits (agentrouter.Batching.Validate). */
export function validBatching(s: RouterSettingsInput): boolean {
  const { minSeconds, maxSeconds, maxRunsPerHourLimit } = ROUTER_BATCHING;
  const inRange = (n: number, lo: number, hi: number) => Number.isInteger(n) && n >= lo && n <= hi;
  return (
    inRange(s.debounce_seconds, minSeconds, maxSeconds) &&
    inRange(s.max_wait_seconds, minSeconds, maxSeconds) &&
    s.max_wait_seconds >= s.debounce_seconds &&
    inRange(s.max_runs_per_hour, 1, maxRunsPerHourLimit)
  );
}

export function validSettings(s: RouterSettingsInput): boolean {
  return s.event_types.length > 0 && validBatching(s);
}

/** Whether any subscription differs from the form, so Save has work to do. */
export function settingsChanged(subs: RouterSubscription[], s: RouterSettingsInput): boolean {
  return subs.some(
    (sub) =>
      sub.debounce_seconds !== s.debounce_seconds ||
      sub.max_wait_seconds !== s.max_wait_seconds ||
      sub.max_runs_per_hour !== s.max_runs_per_hour ||
      [...sub.event_types].sort().join() !== [...s.event_types].sort().join(),
  );
}
