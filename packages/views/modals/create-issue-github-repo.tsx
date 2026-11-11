// Added by the mouse9527/multica fork: choosing the GitHub repository of a new
// GitHub-native issue in the create dialog. Upstream has no such file.
"use client";

import { useEffect, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { toast } from "sonner";
import { FolderGit, HardDrive, Loader2, RotateCw } from "@multica/ui/icons";
import {
  defaultGitHubCreateRepo,
  projectGitHubSettingsOptions,
  type GitHubCreateTarget,
} from "@multica/core/github-issues";
import { PropertyPicker, PickerItem, PickerEmpty } from "../issues/components/pickers/property-picker";
import { PillButton } from "../common/pill-button";
import { useIssueStatuses } from "@multica/core/issue-statuses/hooks";
import { useT } from "../i18n";

const LOCAL = "local";

/**
 * Where the project's repositories stand:
 * - none: no project, a create that cannot go to GitHub, a project without
 *   GitHub repositories, or a server with the feature off. The create is
 *   upstream's.
 * - loading / error: not known yet. The server would choose a repository on
 *   its own, so the dialog does not create until it knows.
 * - ready: the project has repositories; the picker shows.
 */
export type GitHubCreateRepoPhase = "none" | "loading" | "error" | "ready";

export interface GitHubCreateRepoState {
  phase: GitHubCreateRepoPhase;
  repos: string[];
  /** The selected repository ("owner/name"), "local", or null for none yet. */
  selected: string | null;
  select: (value: string) => void;
  /** The create request fields; undefined while nothing is selected. */
  target: GitHubCreateTarget | undefined;
  /** Statuses a new GitHub issue of the project can start in, entry first;
   *  empty when the server does not say. */
  createStatuses: string[];
  retry: () => void;
}

/**
 * The repository a new issue in projectId goes to. Until the user picks, the
 * project's default (else its only repository) is selected; switching projects
 * drops the pick.
 */
export function useGitHubCreateRepo(wsId: string, projectId: string | undefined, enabled = true): GitHubCreateRepoState {
  const active = enabled && !!projectId;
  const query = useQuery({
    ...projectGitHubSettingsOptions(wsId, projectId ?? ""),
    enabled: active,
  });
  const [picked, setPicked] = useState<{ projectId: string | undefined; value: string } | null>(null);
  const settings = query.data;
  let phase: GitHubCreateRepoPhase = "none";
  if (active) {
    if (query.isPending) phase = "loading";
    else if (query.isError) phase = "error";
    else if (settings && settings.repos.length > 0) phase = "ready";
  }
  const ready = phase === "ready";
  const selected = !ready
    ? null
    : picked && picked.projectId === projectId
      ? picked.value
      : defaultGitHubCreateRepo(settings);
  let target: GitHubCreateTarget | undefined;
  if (selected === LOCAL) target = { provider: "local" };
  else if (selected) target = { repo: selected };
  return {
    phase,
    repos: ready ? (settings?.repos.map((r) => r.repo) ?? []) : [],
    selected,
    select: (value) => setPicked({ projectId, value }),
    target,
    createStatuses: ready ? (settings?.create_statuses ?? []) : [],
    retry: () => void query.refetch(),
  };
}

/** Why the dialog must not create yet, or null when it may. */
export type GitHubCreateBlock =
  | { kind: "loading" }
  | { kind: "error" }
  | { kind: "choose" }
  | { kind: "status"; allowed: string[] };

export function gitHubCreateBlock(state: GitHubCreateRepoState, status: string): GitHubCreateBlock | null {
  switch (state.phase) {
    case "none":
      return null;
    case "loading":
      return { kind: "loading" };
    case "error":
      return { kind: "error" };
  }
  if (!state.target) return { kind: "choose" };
  if ("repo" in state.target && state.createStatuses.length > 0 && !state.createStatuses.includes(status)) {
    return { kind: "status", allowed: state.createStatuses };
  }
  return null;
}

/**
 * The dialog's pre-submit check: returns true, with a toast saying why, when
 * the create must wait. A failed read is retried; a missing choice opens the
 * picker through onChoose.
 */
export function useGitHubCreateGate(wsId: string, state: GitHubCreateRepoState, status: string, onChoose: () => void) {
  const { t } = useT("modals");
  const { labelOf } = useIssueStatuses(wsId);
  return (): boolean => {
    const block = gitHubCreateBlock(state, status);
    if (!block) return false;
    switch (block.kind) {
      case "loading":
        toast.error(t(($) => $.create_issue.github_repo.blocked_loading));
        break;
      case "error":
        toast.error(t(($) => $.create_issue.github_repo.blocked_error));
        state.retry();
        break;
      case "choose":
        toast.error(t(($) => $.create_issue.github_repo.required));
        onChoose();
        break;
      case "status":
        toast.error(
          t(($) => $.create_issue.github_repo.status_not_creatable, {
            status: labelOf(status),
            allowed: block.allowed.map(labelOf).join(", "),
          }),
        );
        break;
    }
    return true;
  };
}

/**
 * Starts a new GitHub issue in a status GitHub can show. Whenever the
 * destination becomes a repository (preselected or picked) and the current
 * status is not one a new GitHub issue can take, the status moves to the
 * project's entry status. A status picked afterwards is left alone;
 * gitHubCreateBlock explains it before submit.
 */
export function useGitHubCreateStatus(
  state: GitHubCreateRepoState,
  status: string,
  setStatus: (status: string) => void,
) {
  const repo = state.target && "repo" in state.target ? state.target.repo : null;
  const allowed = state.createStatuses;
  const key = repo && allowed.length > 0 ? `${repo}\n${allowed.join(",")}` : "";
  const [appliedKey, setAppliedKey] = useState("");
  useEffect(() => {
    if (key === appliedKey) return;
    setAppliedKey(key);
    if (key && !allowed.includes(status)) setStatus(allowed[0]!);
  }, [key, appliedKey, allowed, status, setStatus]);
}

export function GitHubRepoPicker({
  state,
  open,
  onOpenChange,
}: {
  state: GitHubCreateRepoState;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const { t } = useT("modals");
  const [filter, setFilter] = useState("");

  if (state.phase === "loading") {
    return (
      <PillButton disabled aria-busy>
        <Loader2 className="h-3.5 w-3.5 shrink-0 animate-spin text-muted-foreground" />
        <span className="truncate text-muted-foreground">{t(($) => $.create_issue.github_repo.loading)}</span>
      </PillButton>
    );
  }
  if (state.phase === "error") {
    return (
      <PillButton onClick={state.retry}>
        <RotateCw className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
        <span className="truncate">{t(($) => $.create_issue.github_repo.load_failed)}</span>
      </PillButton>
    );
  }
  if (state.phase !== "ready") return null;

  const query = filter.trim().toLowerCase();
  const filtered = state.repos.filter((repo) => repo.toLowerCase().includes(query));
  const choose = (value: string) => {
    state.select(value);
    onOpenChange(false);
  };

  return (
    <PropertyPicker
      open={open}
      onOpenChange={onOpenChange}
      width="w-60"
      align="start"
      searchable={state.repos.length > 5}
      searchPlaceholder={t(($) => $.create_issue.github_repo.search_placeholder)}
      onSearchChange={setFilter}
      triggerRender={<PillButton />}
      tooltip={t(($) => $.create_issue.github_repo.tooltip)}
      trigger={
        state.selected === LOCAL ? (
          <>
            <HardDrive className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
            <span className="truncate">{t(($) => $.create_issue.github_repo.local)}</span>
          </>
        ) : (
          <>
            <FolderGit className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
            <span className={state.selected ? "truncate" : "truncate text-muted-foreground"}>
              {state.selected ?? t(($) => $.create_issue.github_repo.choose)}
            </span>
          </>
        )
      }
    >
      {filtered.map((repo) => (
        <PickerItem key={repo} selected={state.selected === repo} truncationTooltip={repo} onClick={() => choose(repo)}>
          <FolderGit className="h-3.5 w-3.5 text-muted-foreground" />
          <span className="truncate">{repo}</span>
        </PickerItem>
      ))}
      {query && filtered.length === 0 && <PickerEmpty />}
      <PickerItem selected={state.selected === LOCAL} onClick={() => choose(LOCAL)}>
        <HardDrive className="h-3.5 w-3.5 text-muted-foreground" />
        <span className="truncate">{t(($) => $.create_issue.github_repo.local)}</span>
      </PickerItem>
    </PropertyPicker>
  );
}
