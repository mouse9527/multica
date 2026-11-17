// Added by the mouse9527/multica fork: Router marker in the chat list (M-108).

package agentrouter

import (
	"context"
	"slices"
)

// ChatSummary is what the chat list shows of one Router: a chat with at
// least one subscription bound to it, its scopes and whether it runs.
type ChatSummary struct {
	ChatSessionID string `json:"chat_session_id"`
	// PausedReasons are the distinct reasons of its paused scopes, in scope
	// order; empty while every scope runs.
	PausedReasons []string           `json:"paused_reasons"`
	Scopes        []ChatSummaryScope `json:"scopes"`
}

// ChatSummaryScope is one subscription of a Router.
type ChatSummaryScope struct {
	SubscriptionID string `json:"subscription_id"`
	ScopeType      string `json:"scope_type"`
	ScopeID        string `json:"scope_id"`
	// Name is the project title or the root issue's title, and Identifier
	// the root issue's identifier. Both are empty when the scope is gone or
	// lies in a project the member cannot see (Visible false).
	Name       string `json:"name"`
	Identifier string `json:"identifier"`
	Visible    bool   `json:"visible"`
	// PausedReason is "manual" for a scope paused by hand, "rate" or
	// "agent_unavailable" when dispatch stopped, and null while it runs.
	PausedReason *string `json:"paused_reason"`
}

// ChatSummaries returns the Routers of the member's own chats in workspaceID.
// A binding requires the chat's creator to be the subscription's owner, so
// these are the Routers that answer to the member. Names in hiddenProjects
// are left out.
func (s *Store) ChatSummaries(ctx context.Context, workspaceID, userID string, hiddenProjects []string) ([]ChatSummary, error) {
	rows, err := s.DB.Query(ctx, `
		SELECT ca.chat_session_id::text, s.id::text, s.scope_type, s.scope_id::text,
		       s.enabled, s.paused_reason,
		       COALESCE(p.id, i.project_id)::text,
		       COALESCE(p.title, i.title, ''),
		       COALESCE(w.issue_prefix || '-' || i.number, ''),
		       (p.id IS NOT NULL OR i.id IS NOT NULL)
		FROM router_chat_anchor ca
		JOIN router_subscription s ON s.id = ca.subscription_id AND s.workspace_id = $1::uuid
		JOIN chat_session c ON c.id = ca.chat_session_id AND c.workspace_id = $1::uuid
		LEFT JOIN project p ON s.scope_type = 'project' AND p.id = s.scope_id AND p.workspace_id = $1::uuid
		LEFT JOIN issue i ON s.scope_type = 'issue_tree' AND i.id = s.scope_id AND i.workspace_id = $1::uuid
		LEFT JOIN workspace w ON w.id = i.workspace_id
		WHERE ca.workspace_id = $1::uuid AND c.creator_id = $2::uuid
		ORDER BY ca.chat_session_id, s.created_at, s.id`, workspaceID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChatSummary{}
	for rows.Next() {
		var (
			chatID         string
			projectID      *string
			enabled, found bool
			sc             ChatSummaryScope
		)
		if err := rows.Scan(&chatID, &sc.SubscriptionID, &sc.ScopeType, &sc.ScopeID,
			&enabled, &sc.PausedReason, &projectID, &sc.Name, &sc.Identifier, &found); err != nil {
			return nil, err
		}
		if !enabled && sc.PausedReason == nil {
			manual := "manual"
			sc.PausedReason = &manual
		}
		sc.Visible = found && (projectID == nil || !slices.Contains(hiddenProjects, *projectID))
		if !sc.Visible {
			sc.Name, sc.Identifier = "", ""
		}
		if len(out) == 0 || out[len(out)-1].ChatSessionID != chatID {
			out = append(out, ChatSummary{ChatSessionID: chatID, PausedReasons: []string{}})
		}
		cur := &out[len(out)-1]
		cur.Scopes = append(cur.Scopes, sc)
		if sc.PausedReason != nil && !slices.Contains(cur.PausedReasons, *sc.PausedReason) {
			cur.PausedReasons = append(cur.PausedReasons, *sc.PausedReason)
		}
	}
	return out, rows.Err()
}
