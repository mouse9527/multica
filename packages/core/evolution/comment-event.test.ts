// @vitest-environment node
// Added by the mouse9527/multica fork: Evolution comment events (M-229).
//
// An apply or rollback comments on the proposal issue and pushes that comment
// over comment:created. The open issue page inserts the pushed comment into
// its timeline as is, and the deliverables panel sorts the timeline by
// created_at. These fixtures mirror what `commentEventPayload` in
// server/internal/evolution/apply.go sends (its key set is checked against
// the comments API in server/internal/handler/evolution_apply_test.go).
import { describe, expect, it } from "vitest";
import { mergeDeliverableSources } from "../attachments/deliverables";
import { commentToTimelineEntry } from "../issues/comment-thread";
import type { Comment, TimelineEntry } from "../types";

const earlier: TimelineEntry = {
  type: "comment",
  id: "c-1",
  actor_type: "agent",
  actor_id: "evolution",
  content: "Proposal",
  created_at: "2026-11-24T02:00:00Z",
};

const pushedOnApply = {
  id: "c-2",
  issue_id: "issue-1",
  author_type: "member",
  author_id: "owner",
  content: "Applied as version 1.",
  type: "comment",
  parent_id: null,
  created_at: "2026-11-24T03:00:00Z",
  updated_at: "2026-11-24T03:00:00Z",
  revision: 1,
  resolved_at: null,
  resolved_by_type: null,
  resolved_by_id: null,
  source_task_id: null,
  reactions: [],
  attachments: [],
} as unknown as Comment;

describe("the comment an apply pushes", () => {
  it("sorts into an open issue page's timeline", () => {
    const merged = mergeDeliverableSources([], [earlier, commentToTimelineEntry(pushedOnApply)]);
    expect(merged.map((entry) => entry.id)).toEqual(["c-1", "c-2"]);
  });

  it("crashed the page while it had no created_at", () => {
    const partial: Partial<Comment> = { ...pushedOnApply };
    delete partial.created_at;
    delete partial.updated_at;
    expect(() =>
      mergeDeliverableSources([], [earlier, commentToTimelineEntry(partial as Comment)]),
    ).toThrow(/localeCompare/);
  });
});
