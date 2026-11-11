// Added by the mouse9527/multica fork: choosing the GitHub repository of a new
// GitHub-native issue in the create dialog. Upstream has no such file.
"use client";

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { FolderGit, HardDrive } from "@multica/ui/icons";
import {
  defaultGitHubCreateRepo,
  projectGitHubSettingsOptions,
  type GitHubCreateTarget,
} from "@multica/core/github-issues";
import { PropertyPicker, PickerItem, PickerEmpty } from "../issues/components/pickers/property-picker";
import { PillButton } from "../common/pill-button";
import { useT } from "../i18n";

const LOCAL = "local";

export interface GitHubCreateRepoState {
  /** True when the project has GitHub repositories, so the picker shows. */
  visible: boolean;
  repos: string[];
  /** The selected repository ("owner/name"), "local", or null for none yet. */
  selected: string | null;
  select: (value: string) => void;
  /** The create request fields; undefined while nothing is selected. */
  target: GitHubCreateTarget | undefined;
}

/**
 * The repository a new issue in projectId goes to. Until the user picks, the
 * project's default (else its only repository) is selected; switching projects
 * drops the pick. A server with GitHub-native issues off offers nothing, and
 * the create request stays as upstream's.
 */
export function useGitHubCreateRepo(wsId: string, projectId: string | undefined, enabled = true): GitHubCreateRepoState {
  const { data: settings } = useQuery({
    ...projectGitHubSettingsOptions(wsId, projectId ?? ""),
    enabled: enabled && !!projectId,
  });
  const [picked, setPicked] = useState<{ projectId: string | undefined; value: string } | null>(null);
  const repos = enabled && projectId ? (settings?.repos.map((r) => r.repo) ?? []) : [];
  const visible = repos.length > 0;
  const selected = !visible
    ? null
    : picked && picked.projectId === projectId
      ? picked.value
      : defaultGitHubCreateRepo(settings);
  let target: GitHubCreateTarget | undefined;
  if (selected === LOCAL) target = { provider: "local" };
  else if (selected) target = { repo: selected };
  return {
    visible,
    repos,
    selected,
    select: (value) => setPicked({ projectId, value }),
    target,
  };
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
