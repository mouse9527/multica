// Modified for the mouse9527/multica fork: Router MVP, slice S1 (M-85), and Router v6 slice 2 (M-97).

package agentrouter

import (
	"sort"
	"time"
)

// scopeSub is the part of an enabled subscription the listener needs to match
// an event. The listener keeps these in its cache.
type scopeSub struct {
	ID         string
	AgentID    string
	ScopeType  string
	ScopeID    string
	EventTypes []string
	CreatedAt  time.Time
}

func (s scopeSub) wants(kind string) bool {
	for _, k := range s.EventTypes {
		if k == kind {
			return true
		}
	}
	return false
}

// pickNarrowest returns, for each agent, the one subscription that owns an
// issue. chain is the issue followed by its ancestors, nearest first (at most
// MaxScopeDepth parents); projectID is the issue's own project, or "".
//
// The narrowest scope wins: an issue tree rooted closer to the issue beats one
// rooted further up, and any issue tree beats a project. Ties, which only an
// unusual data shape can produce, go to the earliest subscription. The SQL in
// membersCTE implements the same rule for reconcile; keep them in step.
func pickNarrowest(subs []scopeSub, chain []string, projectID string) []scopeSub {
	depth := chainDepths(chain)
	type ranked struct {
		sub  scopeSub
		rank int
	}
	best := map[string]ranked{}
	for _, s := range subs {
		rank := scopeRank(s, depth, projectID)
		if rank < 0 {
			continue
		}
		cur, ok := best[s.AgentID]
		if !ok || narrower(s, rank, cur.sub, cur.rank) {
			best[s.AgentID] = ranked{sub: s, rank: rank}
		}
	}
	out := make([]scopeSub, 0, len(best))
	for _, r := range best {
		out = append(out, r.sub)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// chainDepths maps each issue of an ancestor chain to its distance from the
// issue, nearest first.
func chainDepths(chain []string) map[string]int {
	depth := make(map[string]int, len(chain))
	for i, id := range chain {
		if _, seen := depth[id]; !seen {
			depth[id] = i
		}
	}
	return depth
}

// scopeRank is how narrowly s covers an issue: the depth of its root in the
// issue's chain, projectRank for its project, or -1 when s does not cover it.
func scopeRank(s scopeSub, depth map[string]int, projectID string) int {
	switch s.ScopeType {
	case ScopeIssueTree:
		if d, ok := depth[s.ScopeID]; ok {
			return d
		}
	case ScopeProject:
		if projectID != "" && s.ScopeID == projectID {
			return projectRank
		}
	}
	return -1
}

// narrower reports whether a (with rank ra) beats b (with rank rb): the lower
// rank, then the earlier subscription.
func narrower(a scopeSub, ra int, b scopeSub, rb int) bool {
	return ra < rb || (ra == rb && (a.CreatedAt.Before(b.CreatedAt) ||
		(a.CreatedAt.Equal(b.CreatedAt) && a.ID < b.ID)))
}
