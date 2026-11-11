// Added by the mouse9527/multica fork: GitHub-native issues. Upstream has no
// such file.
import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";
import type { ProjectGitHubSettings } from "./schema";

export const githubIssueKeys = {
  all: (wsId: string) => ["github-issues", wsId] as const,
  projectSettings: (wsId: string, projectId: string) =>
    [...githubIssueKeys.all(wsId), "project-settings", projectId] as const,
};

/**
 * The GitHub repositories of a project, or null when the server has
 * GitHub-native issues off (or predates them).
 */
export function projectGitHubSettingsOptions(wsId: string, projectId: string) {
  return queryOptions({
    queryKey: githubIssueKeys.projectSettings(wsId, projectId),
    queryFn: () => api.getProjectGitHubSettings(projectId),
    staleTime: 60_000,
  });
}

/** Where a new issue goes: a repository ("owner/name") or a local issue. */
export type GitHubCreateTarget = { repo: string } | { provider: "local" };

/**
 * The repository the create dialog selects before the user picks one: the
 * project's default, else its only repository, else none (the user must
 * choose). Mirrors the server's order for members.
 */
export function defaultGitHubCreateRepo(settings: ProjectGitHubSettings | null | undefined): string | null {
  if (!settings || settings.repos.length === 0) return null;
  const byDefault = settings.repos.find((r) => r.resource_id === settings.default_resource_id);
  if (byDefault) return byDefault.repo;
  return settings.repos.length === 1 ? settings.repos[0]!.repo : null;
}
