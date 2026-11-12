// Added by the mouse9527/multica fork: the GitHub issue behind a GitHub-native
// issue, in the issue detail sidebar. Upstream has no such file.
"use client";

import { toast } from "sonner";
import { Copy, ExternalLink } from "@multica/ui/icons";
import { IconButton } from "@multica/ui/components/common/icon-button";
import { copyText } from "@multica/ui/lib/clipboard";
import { githubIssueLink } from "@multica/core/github-issues";
import type { Issue } from "@multica/core/types";
import { PropRow } from "../../common/prop-row";
import { useT } from "../../i18n";

/** Renders nothing for a local issue. */
export function GitHubIssueLinkRow({ issue }: { issue: Pick<Issue, "identifier" | "provider"> }) {
  const { t } = useT("issues");
  const link = githubIssueLink(issue);
  if (!link) return null;

  const copy = async () => {
    const ok = await copyText(link.url);
    if (ok) toast.success(t(($) => $.detail.link_copied));
    else toast.error(t(($) => $.detail.link_copy_failed));
  };

  return (
    <PropRow label={t(($) => $.detail.prop_github)}>
      <a
        href={link.url}
        target="_blank"
        rel="noreferrer noopener"
        className="flex min-w-0 items-center gap-1 rounded-sm hover:underline"
      >
        <span className="truncate">{link.label}</span>
        <ExternalLink aria-hidden="true" className="size-3 shrink-0 text-muted-foreground" />
      </a>
      <IconButton size="icon-xs" label={t(($) => $.detail.github_copy_link)} onClick={() => void copy()}>
        <Copy />
      </IconButton>
    </PropRow>
  );
}
