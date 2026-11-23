// Added by the mouse9527/multica fork: which Super Agents cover an issue (M-194).

package agentrouter

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"time"
)

// An issue is covered by every subscription whose scope contains it, but each
// agent receives its events from one of them only, the narrowest
// (pickNarrowest). Different agents each receive them. The functions here
// answer three questions about that rule: which subscriptions cover an issue
// (Covering), whether an issue a Super Agent chat just created reaches that
// chat (CreatedIssueScope), and which other agents already receive the events
// of a scope about to be subscribed (Overlaps).

// coverSub is a subscription with what the three answers show about it.
type coverSub struct {
	scopeSub
	Enabled       bool
	PausedReason  *string
	OwnerUserID   *string
	AgentName     string
	ChatSessionID *string
	ChatTitle     string
	ChatCreatorID string
	// ScopeProjectID is the project the scope lives in: the project itself,
	// or the project of an issue tree's root. ScopeFound is false when the
	// project or root issue no longer exists.
	ScopeProjectID *string
	ScopeName      string
	ScopeFound     bool
}

// loadCoverSubs reads a workspace's subscriptions, paused ones included, of
// one agent or (agentID "") of every agent.
func (s *Store) loadCoverSubs(ctx context.Context, workspaceID, agentID string) ([]coverSub, error) {
	rows, err := s.DB.Query(ctx, `
		SELECT s.id::text, s.agent_id::text, s.scope_type, s.scope_id::text, s.event_types, s.created_at,
		       s.enabled, s.paused_reason, `+subscriptionOwnerSQL+`::text, COALESCE(a.name, ''),
		       ca.chat_session_id::text, COALESCE(cs.title, ''), COALESCE(cs.creator_id::text, ''),
		       COALESCE(p.id, i.project_id)::text,
		       COALESCE(p.title, w.issue_prefix || '-' || i.number, ''),
		       (p.id IS NOT NULL OR i.id IS NOT NULL)
		FROM router_subscription s
		LEFT JOIN agent a ON a.id = s.agent_id
		LEFT JOIN router_chat_anchor ca ON ca.subscription_id = s.id
		LEFT JOIN chat_session cs ON cs.id = ca.chat_session_id
		LEFT JOIN project p ON s.scope_type = 'project' AND p.id = s.scope_id AND p.workspace_id = $1::uuid
		LEFT JOIN issue i ON s.scope_type = 'issue_tree' AND i.id = s.scope_id AND i.workspace_id = $1::uuid
		LEFT JOIN workspace w ON w.id = i.workspace_id
		WHERE s.workspace_id = $1::uuid AND ($2 = '' OR s.agent_id = NULLIF($2, '')::uuid)
		ORDER BY s.created_at, s.id`, workspaceID, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []coverSub
	for rows.Next() {
		var c coverSub
		if err := rows.Scan(&c.ID, &c.AgentID, &c.ScopeType, &c.ScopeID, &c.EventTypes, &c.CreatedAt,
			&c.Enabled, &c.PausedReason, &c.OwnerUserID, &c.AgentName,
			&c.ChatSessionID, &c.ChatTitle, &c.ChatCreatorID,
			&c.ScopeProjectID, &c.ScopeName, &c.ScopeFound); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// issuePlace is where an issue sits for scope matching: itself and its
// ancestors, nearest first, and its project ("" for none).
type issuePlace struct {
	chain     []string
	projectID string
}

func loadIssuePlace(ctx context.Context, q Querier, workspaceID, issueID string) (issuePlace, error) {
	issue, err := loadIssue(ctx, q, workspaceID, issueID)
	if err != nil {
		return issuePlace{}, err
	}
	chain, err := ancestorChain(ctx, q, issue.ID)
	if err != nil {
		return issuePlace{}, err
	}
	p := issuePlace{chain: chain}
	if issue.ProjectID != nil {
		p.projectID = *issue.ProjectID
	}
	return p, nil
}

// owners returns, per agent, the enabled subscription that receives the
// issue's events.
func (p issuePlace) owners(subs []coverSub) map[string]string {
	enabled := make([]scopeSub, 0, len(subs))
	for _, c := range subs {
		if c.Enabled {
			enabled = append(enabled, c.scopeSub)
		}
	}
	out := map[string]string{}
	for _, s := range pickNarrowest(enabled, p.chain, p.projectID) {
		out[s.AgentID] = s.ID
	}
	return out
}

// Covering is one subscription whose scope contains an issue.
type Covering struct {
	SubscriptionID string  `json:"subscription_id"`
	AgentID        string  `json:"agent_id"`
	AgentName      string  `json:"agent_name"`
	OwnerUserID    *string `json:"owner_user_id"`
	ScopeType      string  `json:"scope_type"`
	ScopeID        string  `json:"scope_id"`
	// ScopeName is the project's title or the tree root's identifier; ""
	// when the reader cannot see it.
	ScopeName string `json:"scope_name"`
	// ChatSessionID and ChatTitle name the Super Agent chat the subscription
	// is bound to. Only the chat's creator can read a chat, so for anyone
	// else they are null and ChatBound says whether there is one.
	ChatBound     bool    `json:"chat_bound"`
	ChatSessionID *string `json:"chat_session_id"`
	ChatTitle     *string `json:"chat_title"`
	Enabled       bool    `json:"enabled"`
	PausedReason  *string `json:"paused_reason"`
	// Effective is whether this subscription delivers the issue's events to
	// its agent. A running subscription that does not is SupersededBy the
	// agent's narrower one; a paused one delivers nothing.
	Effective    bool    `json:"effective"`
	SupersededBy *string `json:"superseded_by"`
}

// Covering lists the subscriptions whose scope contains issueID, the ones
// that deliver its events first. readerUserID decides which chats are shown;
// scope names in hiddenProjects are left out.
func (s *Store) Covering(ctx context.Context, workspaceID, issueID, readerUserID string, hiddenProjects []string) ([]Covering, error) {
	place, err := loadIssuePlace(ctx, s.DB, workspaceID, issueID)
	if err != nil {
		return nil, err
	}
	subs, err := s.loadCoverSubs(ctx, workspaceID, "")
	if err != nil {
		return nil, err
	}
	owners := place.owners(subs)
	depth := chainDepths(place.chain)
	type ranked struct {
		Covering
		rank      int
		createdAt time.Time
	}
	var found []ranked
	for _, c := range subs {
		rank := scopeRank(c.scopeSub, depth, place.projectID)
		if rank < 0 {
			continue
		}
		cov := Covering{
			SubscriptionID: c.ID,
			AgentID:        c.AgentID,
			AgentName:      c.AgentName,
			OwnerUserID:    c.OwnerUserID,
			ScopeType:      c.ScopeType,
			ScopeID:        c.ScopeID,
			ChatBound:      c.ChatSessionID != nil,
			Enabled:        c.Enabled,
			PausedReason:   c.PausedReason,
			Effective:      owners[c.AgentID] == c.ID,
		}
		if c.scopeVisible(hiddenProjects) {
			cov.ScopeName = c.ScopeName
		}
		if c.ChatSessionID != nil && readerUserID != "" && c.ChatCreatorID == readerUserID {
			title := c.ChatTitle
			cov.ChatSessionID, cov.ChatTitle = c.ChatSessionID, &title
		}
		if owner, ok := owners[c.AgentID]; ok && c.Enabled && owner != c.ID {
			cov.SupersededBy = &owner
		}
		found = append(found, ranked{Covering: cov, rank: rank, createdAt: c.CreatedAt})
	}
	sort.SliceStable(found, func(i, j int) bool {
		a, b := found[i], found[j]
		if a.Effective != b.Effective {
			return a.Effective
		}
		if a.AgentName != b.AgentName {
			return a.AgentName < b.AgentName
		}
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		return a.createdAt.Before(b.createdAt)
	})
	out := make([]Covering, len(found))
	for i, f := range found {
		out[i] = f.Covering
	}
	return out, nil
}

func (c coverSub) scopeVisible(hiddenProjects []string) bool {
	return c.ScopeFound && (c.ScopeProjectID == nil || !slices.Contains(hiddenProjects, *c.ScopeProjectID))
}

// CreatedIssueScope tells a Super Agent chat whether an issue it just
// created reaches it. Batches leave out the agent's own actions, so the
// chat never hears of the new issue from a batch and has to be told when
// the issue's later events will not reach it either.
type CreatedIssueScope struct {
	ChatSessionID string `json:"chat_session_id"`
	// InScope is whether the issue's events go to this chat.
	InScope bool `json:"in_scope"`
	// SubscriptionID is the agent's subscription that receives the issue's
	// events, when one does.
	SubscriptionID *string `json:"subscription_id"`
	// OtherChatSessionID is the chat that subscription is bound to when it is
	// not this one.
	OtherChatSessionID *string `json:"other_chat_session_id"`
	// Hint says what to do when the issue is not in scope; "" when it is.
	Hint string `json:"hint,omitempty"`
}

// CreatedIssueScope checks issueID against the subscriptions of agentID. It
// returns nil when no subscription of the agent is bound to chatSessionID,
// i.e. the chat is not a Super Agent. identifier names the issue in the hint.
func (s *Store) CreatedIssueScope(ctx context.Context, workspaceID, agentID, chatSessionID, issueID, identifier string) (*CreatedIssueScope, error) {
	subs, err := s.loadCoverSubs(ctx, workspaceID, agentID)
	if err != nil {
		return nil, err
	}
	bound := slices.ContainsFunc(subs, func(c coverSub) bool {
		return c.ChatSessionID != nil && *c.ChatSessionID == chatSessionID
	})
	if !bound {
		return nil, nil
	}
	place, err := loadIssuePlace(ctx, s.DB, workspaceID, issueID)
	if err != nil {
		return nil, err
	}
	out := &CreatedIssueScope{ChatSessionID: chatSessionID}
	ownerID, ok := place.owners(subs)[agentID]
	if !ok {
		out.Hint = fmt.Sprintf("%s is not in this Super Agent's scope: no subscription of it covers the issue, "+
			"so its batches will not report it. Tell the owner and ask whether to add it; only after they agree, run "+
			"`multica router subscribe --issue %s --anchor-chat current`.", identifier, identifier)
		return out, nil
	}
	out.SubscriptionID = &ownerID
	i := slices.IndexFunc(subs, func(c coverSub) bool { return c.ID == ownerID })
	other := subs[i].ChatSessionID
	if other != nil && *other == chatSessionID {
		out.InScope = true
		return out, nil
	}
	out.OtherChatSessionID = other
	where := "is not bound to a chat"
	if other != nil {
		where = "is bound to another chat (" + *other + ")"
	}
	out.Hint = fmt.Sprintf("%s is covered by subscription %s of this agent, which %s, so its events go there and not to this chat. "+
		"Tell the owner; change scopes only after they agree.", identifier, ownerID, where)
	return out, nil
}

// Overlap is another agent's subscription that already receives the events
// of issues in a scope about to be subscribed.
type Overlap struct {
	SubscriptionID string `json:"subscription_id"`
	AgentID        string `json:"agent_id"`
	AgentName      string `json:"agent_name"`
	ScopeType      string `json:"scope_type"`
	ScopeID        string `json:"scope_id"`
	ScopeName      string `json:"scope_name"`
	// IssueCount is how many issues of the new scope it receives; Issues
	// names up to maxOverlapIssues of those the reader can see, open ones
	// first.
	IssueCount int      `json:"issue_count"`
	Issues     []string `json:"issues"`
}

const maxOverlapIssues = 10

// Overlaps lists the enabled subscriptions of agents other than agentID that
// receive the events of an issue in the scope (scopeType, scopeID): an issue
// they own today, or the same project. Subscriptions of agentID itself are
// left out, since the narrowest of an agent's subscriptions receives an
// issue's events and the others do not.
func (s *Store) Overlaps(ctx context.Context, workspaceID, agentID, scopeType, scopeID string, hiddenProjects []string) ([]Overlap, error) {
	if hiddenProjects == nil {
		hiddenProjects = []string{}
	}
	rows, err := s.DB.Query(ctx, `
		WITH RECURSIVE `+membersCTE+`,
		new_tree AS (
		    SELECT i.id, 0 AS depth FROM issue i
		    WHERE $3 = 'issue_tree' AND i.id = $4::uuid AND i.workspace_id = $1::uuid
		    UNION ALL
		    SELECT c.id, t.depth + 1 FROM new_tree t JOIN issue c ON c.parent_issue_id = t.id
		    WHERE t.depth < 32
		),
		new_scope AS (
		    SELECT id AS issue_id FROM new_tree
		    UNION
		    SELECT i.id FROM issue i WHERE $3 = 'project' AND i.project_id = $4::uuid AND i.workspace_id = $1::uuid
		),
		hits AS (
		    SELECT m.sub_id, i.number, w.issue_prefix || '-' || i.number AS identifier,
		           i.status IN ('done', 'cancelled') AS closed,
		           (i.project_id IS NULL OR NOT (i.project_id = ANY($5::uuid[]))) AS visible
		    FROM members m
		    JOIN new_scope n ON n.issue_id = m.issue_id
		    JOIN issue i ON i.id = m.issue_id
		    JOIN workspace w ON w.id = i.workspace_id
		    WHERE m.agent_id <> $2::uuid
		)
		SELECT s.id::text,
		       (SELECT count(*) FROM hits h WHERE h.sub_id = s.id),
		       COALESCE((SELECT array_agg(x.identifier ORDER BY x.closed, x.number) FROM (
		           SELECT h.identifier, h.closed, h.number FROM hits h
		           WHERE h.sub_id = s.id AND h.visible
		           ORDER BY h.closed, h.number LIMIT $6) x), '{}')
		FROM router_subscription s
		WHERE s.workspace_id = $1::uuid AND s.enabled AND s.agent_id <> $2::uuid
		  AND (EXISTS (SELECT 1 FROM hits h WHERE h.sub_id = s.id)
		       OR ($3 = 'project' AND s.scope_type = 'project' AND s.scope_id = $4::uuid))
		ORDER BY s.created_at, s.id`,
		workspaceID, agentID, scopeType, scopeID, hiddenProjects, maxOverlapIssues)
	if err != nil {
		return nil, err
	}
	type hit struct {
		count  int
		issues []string
	}
	var ids []string
	hits := map[string]hit{}
	for rows.Next() {
		var id string
		var h hit
		if err := rows.Scan(&id, &h.count, &h.issues); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
		hits[id] = h
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return []Overlap{}, nil
	}
	subs, err := s.loadCoverSubs(ctx, workspaceID, "")
	if err != nil {
		return nil, err
	}
	out := make([]Overlap, 0, len(ids))
	for _, c := range subs {
		h, ok := hits[c.ID]
		if !ok {
			continue
		}
		o := Overlap{
			SubscriptionID: c.ID,
			AgentID:        c.AgentID,
			AgentName:      c.AgentName,
			ScopeType:      c.ScopeType,
			ScopeID:        c.ScopeID,
			IssueCount:     h.count,
			Issues:         h.issues,
		}
		if o.Issues == nil {
			o.Issues = []string{}
		}
		if c.scopeVisible(hiddenProjects) {
			o.ScopeName = c.ScopeName
		}
		out = append(out, o)
	}
	return out, nil
}
