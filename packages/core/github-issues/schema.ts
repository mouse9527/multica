// Added by the mouse9527/multica fork: GitHub-native issues. Upstream has no
// such file.
import { z } from "zod";

/** A GitHub repository registered on a project. */
export interface ProjectGitHubRepo {
  resource_id: string;
  /** "owner/name" */
  repo: string;
}

/** The repositories a new issue of a project can be opened on. */
export interface ProjectGitHubSettings {
  repos: ProjectGitHubRepo[];
  /** The project_resource id of the project's default repository. */
  default_resource_id: string | null;
  /** Statuses a new GitHub issue of the project can start in, entry status
   *  first. Empty from a server that does not say. */
  create_statuses: string[];
}

export const ProjectGitHubSettingsSchema = z.object({
  repos: z
    .array(z.object({ resource_id: z.string(), repo: z.string() }))
    .nullish()
    .transform((v) => v ?? []),
  default_resource_id: z
    .string()
    .nullish()
    .transform((v) => v ?? null),
  create_statuses: z
    .array(z.string())
    .nullish()
    .transform((v) => v ?? []),
});

export const EMPTY_PROJECT_GITHUB_SETTINGS: ProjectGitHubSettings = {
  repos: [],
  default_resource_id: null,
  create_statuses: [],
};
