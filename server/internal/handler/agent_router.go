// Modified for the mouse9527/multica fork: Router MVP, slice S1 (M-85).
//
// HTTP surface of internal/agentrouter. It lives in package handler only to
// reuse the request helpers (actor, workspace, issue and agent loaders,
// private-project filtering); the logic is in the agentrouter package.

package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/agentrouter"
	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// MountAgentRouter registers /api/router. Every route answers 404 while
// router_v1 is off.
func (h *Handler) MountAgentRouter(r chi.Router) {
	r.Use(h.requireRouterV1)
	r.Get("/subscriptions", h.ListRouterSubscriptions)
	r.Post("/subscriptions", h.CreateRouterSubscription)
	r.Get("/subscriptions/{id}", h.GetRouterSubscription)
	r.Post("/subscriptions/{id}/pause", h.PauseRouterSubscription)
	r.Post("/subscriptions/{id}/resume", h.ResumeRouterSubscription)
	r.Delete("/subscriptions/{id}", h.DeleteRouterSubscription)
	r.Get("/subscriptions/{id}/scope", h.GetRouterSubscriptionScope)
	r.Get("/events", h.ListRouterEvents)
}

func (h *Handler) requireRouterV1(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !featureflags.RouterV1Enabled(r.Context(), h.FeatureFlags) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) routerStore() *agentrouter.Store {
	return agentrouter.NewStore(h.DB, h.TxStarter)
}

func (h *Handler) routerFail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, agentrouter.ErrNotFound):
		writeError(w, http.StatusNotFound, "subscription not found")
	case errors.Is(err, agentrouter.ErrDuplicate):
		writeError(w, http.StatusConflict, err.Error())
	default:
		if writeQueryCancellation(w, r, err) {
			return
		}
		slog.Error("router request failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "router request failed")
	}
}

type CreateRouterSubscriptionRequest struct {
	// AgentID defaults to the calling agent when an agent makes the request.
	AgentID   string `json:"agent_id"`
	ScopeType string `json:"scope_type"`
	// ScopeID is an issue id or identifier (issue_tree) or a project id.
	ScopeID    string   `json:"scope_id"`
	EventTypes []string `json:"event_types"`
}

func (h *Handler) CreateRouterSubscription(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if workspaceID == "" {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return
	}
	var req CreateRouterSubscriptionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	scopeType, err := agentrouter.ParseScopeType(req.ScopeType)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	eventTypes, err := agentrouter.NormalizeEventTypes(req.EventTypes)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	userID := requestUserID(r)
	actorType, actorID := h.resolveActor(r, userID, workspaceID)
	agentRef := strings.TrimSpace(req.AgentID)
	if agentRef == "" {
		if actorType != "agent" {
			writeError(w, http.StatusBadRequest, "agent_id is required")
			return
		}
		agentRef = actorID
	}
	agent, ok := h.loadAgentForUser(w, r, agentRef)
	if !ok {
		return
	}
	// An agent may subscribe only itself; a member must be able to manage
	// the agent (its owner or a workspace admin), because later slices run
	// the agent on what it captures.
	if actorType == "agent" {
		if uuidToString(agent.ID) != actorID {
			writeError(w, http.StatusForbidden, "an agent can only subscribe itself")
			return
		}
	} else if !h.canManageAgent(w, r, agent) {
		return
	}

	var scopeID, anchorID string
	switch scopeType {
	case agentrouter.ScopeIssueTree:
		issue, ok := h.loadIssueForUser(w, r, strings.TrimSpace(req.ScopeID))
		if !ok {
			return
		}
		scopeID = uuidToString(issue.ID)
		anchorID = scopeID
	case agentrouter.ScopeProject:
		if !h.loadRouterProject(w, r, workspaceID, req.ScopeID) {
			return
		}
		scopeID = strings.TrimSpace(req.ScopeID)
	}

	sub, err := h.routerStore().Create(r.Context(), agentrouter.CreateParams{
		WorkspaceID:   workspaceID,
		AgentID:       uuidToString(agent.ID),
		OwnerUserID:   userID,
		ScopeType:     scopeType,
		ScopeID:       scopeID,
		AnchorIssueID: anchorID,
		EventTypes:    eventTypes,
		CreatedByType: actorType,
		CreatedByID:   actorID,
	})
	if err != nil {
		h.routerFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, sub)
}

func (h *Handler) loadRouterProject(w http.ResponseWriter, r *http.Request, workspaceID, projectID string) bool {
	id, ok := parseUUIDOrBadRequest(w, strings.TrimSpace(projectID), "scope_id")
	if !ok {
		return false
	}
	project, err := h.Queries.GetProjectInWorkspace(r.Context(), db.GetProjectInWorkspaceParams{
		ID:          id,
		WorkspaceID: parseUUID(workspaceID),
	})
	if err != nil {
		if writeQueryCancellation(w, r, err) {
			return false
		}
		writeError(w, http.StatusNotFound, "project not found")
		return false
	}
	return h.requireProjectVisible(w, r, workspaceID, project.ID, http.StatusNotFound, "project not found")
}

func (h *Handler) ListRouterSubscriptions(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if workspaceID == "" {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return
	}
	agentID := strings.TrimSpace(r.URL.Query().Get("agent_id"))
	if agentID != "" {
		if _, ok := parseUUIDOrBadRequest(w, agentID, "agent_id"); !ok {
			return
		}
	}
	subs, err := h.routerStore().List(r.Context(), workspaceID, agentID)
	if err != nil {
		h.routerFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"subscriptions": subs})
}

// loadRouterSubscription resolves the {id} URL param in the request's
// workspace.
func (h *Handler) loadRouterSubscription(w http.ResponseWriter, r *http.Request) (agentrouter.Subscription, bool) {
	workspaceID := h.resolveWorkspaceID(r)
	if workspaceID == "" {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return agentrouter.Subscription{}, false
	}
	id, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "subscription id")
	if !ok {
		return agentrouter.Subscription{}, false
	}
	sub, err := h.routerStore().Get(r.Context(), workspaceID, uuidToString(id))
	if err != nil {
		h.routerFail(w, r, err)
		return agentrouter.Subscription{}, false
	}
	return sub, true
}

// canChangeRouterSubscription allows the subscribed agent itself, the member
// who created the subscription, and workspace owners and admins.
func (h *Handler) canChangeRouterSubscription(w http.ResponseWriter, r *http.Request, sub agentrouter.Subscription) bool {
	actorType, actorID := h.resolveActor(r, requestUserID(r), sub.WorkspaceID)
	if actorType == "agent" {
		if actorID == sub.AgentID {
			return true
		}
		writeError(w, http.StatusForbidden, "an agent can only change its own subscriptions")
		return false
	}
	if sub.CreatedByType == "member" && sub.CreatedByID == actorID {
		return true
	}
	member, ok := h.workspaceMember(w, r, sub.WorkspaceID)
	if !ok {
		return false
	}
	if !roleAllowed(member.Role, "owner", "admin") {
		writeError(w, http.StatusForbidden, "only the subscription's creator, its agent, or a workspace admin can change it")
		return false
	}
	return true
}

func (h *Handler) GetRouterSubscription(w http.ResponseWriter, r *http.Request) {
	sub, ok := h.loadRouterSubscription(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, sub)
}

func (h *Handler) PauseRouterSubscription(w http.ResponseWriter, r *http.Request) {
	sub, ok := h.loadRouterSubscription(w, r)
	if !ok || !h.canChangeRouterSubscription(w, r, sub) {
		return
	}
	out, err := h.routerStore().Pause(r.Context(), sub.WorkspaceID, sub.ID)
	if err != nil {
		h.routerFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) ResumeRouterSubscription(w http.ResponseWriter, r *http.Request) {
	sub, ok := h.loadRouterSubscription(w, r)
	if !ok || !h.canChangeRouterSubscription(w, r, sub) {
		return
	}
	out, err := h.routerStore().Resume(r.Context(), sub.WorkspaceID, sub.ID)
	if err != nil {
		h.routerFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) DeleteRouterSubscription(w http.ResponseWriter, r *http.Request) {
	sub, ok := h.loadRouterSubscription(w, r)
	if !ok || !h.canChangeRouterSubscription(w, r, sub) {
		return
	}
	if err := h.routerStore().Delete(r.Context(), sub.WorkspaceID, sub.ID); err != nil {
		h.routerFail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) routerHiddenProjects(w http.ResponseWriter, r *http.Request, workspaceID string) ([]string, bool) {
	hidden, ok := h.requestHiddenProjects(w, r, workspaceID)
	if !ok {
		return nil, false
	}
	ids := hidden.IDs()
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, util.UUIDToString(id))
	}
	return out, true
}

func (h *Handler) GetRouterSubscriptionScope(w http.ResponseWriter, r *http.Request) {
	sub, ok := h.loadRouterSubscription(w, r)
	if !ok {
		return
	}
	hidden, ok := h.routerHiddenProjects(w, r, sub.WorkspaceID)
	if !ok {
		return
	}
	includeClosed := r.URL.Query().Get("include_closed") == "true"
	issues, err := h.routerStore().Scope(r.Context(), sub.WorkspaceID, sub.ID, includeClosed, hidden)
	if err != nil {
		h.routerFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"subscription_id": sub.ID, "enabled": sub.Enabled, "issues": issues})
}

func (h *Handler) ListRouterEvents(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if workspaceID == "" {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return
	}
	q := r.URL.Query()
	f := agentrouter.EventFilter{
		WorkspaceID: workspaceID,
		State:       q.Get("state"),
		Source:      q.Get("source"),
	}
	for _, p := range []struct {
		name string
		dst  *string
	}{{"subscription_id", &f.SubscriptionID}, {"agent_id", &f.AgentID}} {
		v := strings.TrimSpace(q.Get(p.name))
		if v == "" {
			continue
		}
		if _, ok := parseUUIDOrBadRequest(w, v, p.name); !ok {
			return
		}
		*p.dst = v
	}
	if f.State != "" && f.State != "pending" && f.State != "consumed" {
		writeError(w, http.StatusBadRequest, "state must be pending or consumed")
		return
	}
	if f.Source != "" && f.Source != agentrouter.SourceBus && f.Source != agentrouter.SourceReconcile {
		writeError(w, http.StatusBadRequest, "source must be bus or reconcile")
		return
	}
	if v := q.Get("since"); v != "" {
		since, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "since must be an RFC 3339 timestamp")
			return
		}
		f.Since = &since
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 500")
			return
		}
		f.Limit = n
	}
	hidden, ok := h.routerHiddenProjects(w, r, workspaceID)
	if !ok {
		return
	}
	f.HiddenProjects = hidden
	evs, err := h.routerStore().ListEvents(r.Context(), f)
	if err != nil {
		h.routerFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": evs})
}
