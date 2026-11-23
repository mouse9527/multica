// Added by the mouse9527/multica fork: which Super Agents cover an issue, the
// scope check on issues a Super Agent chat creates, and the overlap check on
// subscribe (M-194).

package handler

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/agentrouter"
	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/logger"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// GetRouterIssueCovering lists the subscriptions whose scope contains an
// issue, and which of them deliver its events. Chats are named only to the
// member who started them: to the caller, or for an agent to the member who
// started its run.
func (h *Handler) GetRouterIssueCovering(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	workspaceID := uuidToString(issue.WorkspaceID)
	hidden, ok := h.routerHiddenProjects(w, r, workspaceID)
	if !ok {
		return
	}
	reader := ""
	actorType, actorID := h.resolveActor(r, requestUserID(r), workspaceID)
	switch actorType {
	case "member":
		reader = actorID
	case "agent":
		reader = h.routerAgentAuthority(r, workspaceID).userID
	}
	covering, err := h.routerStore().Covering(r.Context(), workspaceID, uuidToString(issue.ID), reader, hidden)
	if err != nil {
		h.routerFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"issue_id": uuidToString(issue.ID), "covering": covering})
}

// superAgentChatRun returns the chat session of the run an agent request
// comes from, or "" when the caller is not an agent in a chat run. The run is
// the one M-193 attributes the agent's actions to (actorTaskID), and its chat
// is read with the same helper.
func (h *Handler) superAgentChatRun(r *http.Request, workspaceID string) (agentID, chatSessionID string) {
	actorType, actorID := h.resolveActor(r, requestUserID(r), workspaceID)
	taskID := actorTaskID(r, actorType)
	if taskID == "" {
		return "", ""
	}
	chat, err := agentrouter.TaskChatSession(r.Context(), h.DB, taskID)
	if err != nil || chat == "" {
		return "", ""
	}
	return actorID, chat
}

// superAgentCreatedIssueScope is the scope check on an issue created from a
// Super Agent chat run: whether the issue's events will reach that chat, and
// what to do when they will not. It is nil for every other create, while
// router_chat is off, and when the check fails, which must not fail the
// create.
func (h *Handler) superAgentCreatedIssueScope(ctx context.Context, r *http.Request, workspaceID string, issue db.Issue, identifier string) *agentrouter.CreatedIssueScope {
	if !featureflags.RouterChatEnabled(ctx, h.FeatureFlags) {
		return nil
	}
	agentID, chatSessionID := h.superAgentChatRun(r, workspaceID)
	if chatSessionID == "" {
		return nil
	}
	out, err := h.routerStore().CreatedIssueScope(ctx, workspaceID, agentID, chatSessionID, uuidToString(issue.ID), identifier)
	if err != nil {
		slog.Warn("router: scope check on a created issue failed; answering without it",
			append(logger.RequestAttrs(r), "issue_id", uuidToString(issue.ID), "chat_session_id", chatSessionID, "error", err)...)
		return nil
	}
	return out
}

// routerScopeOverlapCode marks the 409 a subscribe gets when other agents
// already receive events from the scope. Repeating the request with
// allow_overlap subscribes anyway.
const routerScopeOverlapCode = "router_scope_overlap"

// refuseRouterOverlap answers 409 and returns true when agents other than
// agentID already receive the events of issues in the scope.
func (h *Handler) refuseRouterOverlap(w http.ResponseWriter, r *http.Request, workspaceID, agentID, scopeType, scopeID string) bool {
	hidden, ok := h.routerHiddenProjects(w, r, workspaceID)
	if !ok {
		return true
	}
	overlaps, err := h.routerStore().Overlaps(r.Context(), workspaceID, agentID, scopeType, scopeID, hidden)
	if err != nil {
		h.routerFail(w, r, err)
		return true
	}
	if len(overlaps) == 0 {
		return false
	}
	names := make([]string, 0, len(overlaps))
	for _, o := range overlaps {
		name := o.AgentName
		if name == "" {
			name = o.AgentID
		}
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	writeJSON(w, http.StatusConflict, map[string]any{
		"code": routerScopeOverlapCode,
		"error": "other Super Agents already receive events from this scope (" + strings.Join(names, ", ") +
			"); each would act on the same issues. Confirm with the owner, then repeat with allow_overlap to subscribe anyway",
		"overlaps": overlaps,
	})
	return true
}
