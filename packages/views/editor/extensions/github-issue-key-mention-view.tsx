// Added by the mouse9527/multica fork: the editor chip of a GitHub-native issue
// key (`owner/repo#N`). Upstream has no such file.
"use client";

import { NodeViewWrapper } from "@tiptap/react";
import { IssueMentionCard } from "../../issues/components/issue-mention-card";
import { useResolveIssueIdentifier } from "../../issues/hooks/use-resolve-issue-identifier";

/**
 * Resolves the key against the current workspace like the readonly renderer
 * does: the issue chip on a hit, the key as plain text otherwise (for example
 * after the issue stopped being tracked).
 */
export function GitHubIssueKeyMentionView({ issueKey }: { issueKey: string }) {
  const issue = useResolveIssueIdentifier(issueKey);
  if (!issue) {
    return (
      <NodeViewWrapper as="span" className="inline">
        {issueKey}
      </NodeViewWrapper>
    );
  }
  return (
    <NodeViewWrapper
      as="span"
      className="inline"
      onClick={(e: React.MouseEvent) => e.stopPropagation()}
    >
      <IssueMentionCard issueId={issue.id} fallbackLabel={issueKey} />
    </NodeViewWrapper>
  );
}
