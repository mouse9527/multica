// Modified for the mouse9527/multica fork: Router MVP, slice S2 (M-86), Router v6 slices 1 (M-96) and 2 (M-97), Router settings by instruction (M-105), and Router inbox S3b (M-133).
//
// HTTP surface of Router dispatch: subscription updates, state, the audit
// trail, anchor runs and owner notices. Like agent_router.go it only adapts
// requests; the logic is in internal/agentrouter.

package handler

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/agentrouter"
	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// mountRouterDispatch adds the S2 routes under /api/router. Updating a
// subscription and reading its audit trail and runs need only router_v1;
// state and notices also need router_dispatch.
func (h *Handler) mountRouterDispatch(r chi.Router) {
	r.Patch("/subscriptions/{id}", h.UpdateRouterSubscription)
	r.Get("/subscriptions/{id}/changes", h.ListRouterSubscriptionChanges)
	r.Get("/dispatches", h.ListRouterDispatches)
	r.Group(func(r chi.Router) {
		r.Use(h.requireRouterDispatch)
		r.Get("/subscriptions/{id}/state", h.GetRouterState)
		r.Put("/subscriptions/{id}/state", h.SetRouterState)
		r.Post("/notices", h.CreateOwnerNotice)
		r.Get("/notices", h.ListOwnerNotices)
		r.Get("/holds", h.ListRouterHolds)
		r.Get("/inbox-rules", h.GetInboxRules)
		r.Put("/inbox-rules", h.SetInboxRules)
		r.Get("/inbox-rules/report", h.GetInboxRulesReport)
	})
}

func (h *Handler) requireRouterDispatch(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !featureflags.RouterDispatchEnabled(r.Context(), h.FeatureFlags) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) routerNotifier() *agentrouter.Notifier {
	return agentrouter.NewNotifier(h.routerStore(), agentrouter.InboxAdapter{Queries: h.Queries, Bus: h.Bus})
}

// routerAuthority is the member an agent request acts for: the human who
// started the agent's current run (the task's originator), while that run is
// live. Runs without a human originator (autopilot rule owner, owner
// fallback) carry no authority, so an agent acting on its own can never use
// a member's rights.
type routerAuthority struct {
	userID string
	role   string
	taskID string
}

func (h *Handler) routerAgentAuthority(r *http.Request, workspaceID string) routerAuthority {
	var a routerAuthority
	task, ok := h.taskFromRequestHeader(r)
	if !ok {
		return a
	}
	a.taskID = uuidToString(task.ID)
	if uuidToString(task.AgentID) != r.Header.Get("X-Agent-ID") || isTerminalTaskStatus(task.Status) || !task.OriginatorUserID.Valid {
		return a
	}
	member, err := h.Queries.GetMemberByUserAndWorkspace(r.Context(), db.GetMemberByUserAndWorkspaceParams{
		UserID: task.OriginatorUserID, WorkspaceID: parseUUID(workspaceID),
	})
	if err != nil {
		return a
	}
	a.userID, a.role = uuidToString(task.OriginatorUserID), member.Role
	return a
}

func (a routerAuthority) admin() bool {
	return a.userID != "" && roleAllowed(a.role, "owner", "admin")
}

// canManageAgent mirrors canManageAgent for the run's member.
func (a routerAuthority) canManageAgent(agent db.Agent) bool {
	return a.admin() || (a.userID != "" && uuidToString(agent.OwnerID) == a.userID)
}

// canChangeSubscription mirrors canChangeRouterSubscription's member rule
// for the run's member, except owning the subscribed agent, which the caller
// checks with userOwnsAgent.
func (a routerAuthority) canChangeSubscription(sub agentrouter.Subscription) bool {
	if a.userID == "" {
		return false
	}
	return a.admin() || (sub.CreatedByType == "member" && sub.CreatedByID == a.userID)
}

// userOwnsAgent reports whether userID owns the agent.
func (h *Handler) userOwnsAgent(r *http.Request, agentID, userID string) bool {
	id, err := util.ParseUUID(agentID)
	if err != nil || userID == "" {
		return false
	}
	agent, err := h.Queries.GetAgent(r.Context(), id)
	return err == nil && uuidToString(agent.OwnerID) == userID
}

type UpdateRouterSubscriptionRequest struct {
	EventTypes []string `json:"event_types"`
	// AnchorIssueID is an issue id or identifier.
	AnchorIssueID *string `json:"anchor_issue_id"`
	// AnchorChatSessionID binds the subscription to a chat session; "" or
	// "none" removes the binding. Needs router_chat.
	AnchorChatSessionID *string `json:"anchor_chat_session_id"`
	DebounceSeconds     int     `json:"debounce_seconds"`
	MaxWaitSeconds      int     `json:"max_wait_seconds"`
	MaxRunsPerHour      int     `json:"max_runs_per_hour"`
}

func (h *Handler) UpdateRouterSubscription(w http.ResponseWriter, r *http.Request) {
	sub, ok := h.loadRouterSubscription(w, r)
	if !ok {
		return
	}
	var req UpdateRouterSubscriptionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	change, ok := h.canChangeRouterSubscription(w, r, sub)
	if !ok {
		return
	}
	p := agentrouter.UpdateParams{
		Batching: agentrouter.Batching{
			DebounceSeconds: req.DebounceSeconds, MaxWaitSeconds: req.MaxWaitSeconds, MaxRunsPerHour: req.MaxRunsPerHour,
		},
		Change: change,
	}
	if err := p.Batching.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.EventTypes != nil {
		types, err := agentrouter.NormalizeEventTypes(req.EventTypes)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		p.EventTypes = types
	}
	if req.AnchorIssueID != nil {
		ref := strings.TrimSpace(*req.AnchorIssueID)
		if ref == "" {
			writeError(w, http.StatusBadRequest, "anchor_issue_id must name an issue")
			return
		}
		issue, ok := h.loadIssueForUser(w, r, ref)
		if !ok {
			return
		}
		id := uuidToString(issue.ID)
		p.AnchorIssueID = &id
	}
	if req.AnchorChatSessionID != nil {
		if !featureflags.RouterChatEnabled(r.Context(), h.FeatureFlags) {
			writeError(w, http.StatusBadRequest, "anchor_chat_session_id needs the router_chat feature")
			return
		}
		chat := strings.TrimSpace(*req.AnchorChatSessionID)
		if chat == "none" {
			chat = ""
		}
		if chat != "" {
			id, ok := parseUUIDOrBadRequest(w, chat, "anchor_chat_session_id")
			if !ok {
				return
			}
			chat = uuidToString(id)
			// An agent binds a chat only for that chat's creator (M-105).
			if change.ActorType == "agent" {
				auth := h.routerAgentAuthority(r, sub.WorkspaceID)
				if !h.agentMayBindChat(w, r, auth, sub.WorkspaceID, chat) {
					return
				}
			}
		}
		p.AnchorChatSessionID = &chat
	}
	out, err := h.routerStore().Update(r.Context(), sub.WorkspaceID, sub.ID, p)
	if err != nil {
		h.routerFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) ListRouterSubscriptionChanges(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if workspaceID == "" {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return
	}
	id, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "subscription id")
	if !ok {
		return
	}
	changes, err := h.routerStore().ListChanges(r.Context(), workspaceID, uuidToString(id))
	if err != nil {
		h.routerFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"changes": changes})
}

func (h *Handler) ListRouterDispatches(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if workspaceID == "" {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return
	}
	subID := strings.TrimSpace(r.URL.Query().Get("subscription_id"))
	if subID != "" {
		if _, ok := parseUUIDOrBadRequest(w, subID, "subscription_id"); !ok {
			return
		}
	}
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 200")
			return
		}
		limit = n
	}
	out, err := h.routerStore().ListDispatches(r.Context(), workspaceID, subID, limit)
	if err != nil {
		h.routerFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dispatches": out})
}

func (h *Handler) GetRouterState(w http.ResponseWriter, r *http.Request) {
	sub, ok := h.loadRouterSubscription(w, r)
	if !ok {
		return
	}
	st, err := h.routerStore().GetState(r.Context(), sub.WorkspaceID, sub.ID)
	if err != nil {
		h.routerFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

type SetRouterStateRequest struct {
	State            string `json:"state"`
	ExpectedRevision *int64 `json:"expected_revision"`
}

// SetRouterState replaces the state. Only the subscribed agent and workspace
// owners and admins may write it.
func (h *Handler) SetRouterState(w http.ResponseWriter, r *http.Request) {
	sub, ok := h.loadRouterSubscription(w, r)
	if !ok {
		return
	}
	actorType, actorID := h.resolveActor(r, requestUserID(r), sub.WorkspaceID)
	if actorType == "agent" {
		if actorID != sub.AgentID {
			writeError(w, http.StatusForbidden, "only the subscribed agent can write its state")
			return
		}
	} else {
		member, ok := h.workspaceMember(w, r, sub.WorkspaceID)
		if !ok {
			return
		}
		if !roleAllowed(member.Role, "owner", "admin") {
			writeError(w, http.StatusForbidden, "only the subscribed agent or a workspace admin can write the state")
			return
		}
	}
	var req SetRouterStateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ExpectedRevision == nil {
		writeError(w, http.StatusBadRequest, "expected_revision is required")
		return
	}
	st, err := h.routerStore().SetState(r.Context(), sub.WorkspaceID, sub.ID, req.State, *req.ExpectedRevision)
	if err != nil {
		h.routerFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

type CreateOwnerNoticeRequest struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	// Refs are issue ids or identifiers; the first is the inbox item's issue.
	Refs      []string `json:"refs"`
	Urgency   string   `json:"urgency"`
	DedupeKey string   `json:"dedupe_key"`
	// SubscriptionID routes the notice to that subscription's owner. An
	// agent may name only its own subscriptions.
	SubscriptionID string `json:"subscription_id"`
}

// maxAgentNoticesPerHour bounds how many notices one agent sends an hour.
const maxAgentNoticesPerHour = 30

// CreateOwnerNotice is "notify owner". The recipient is the owner of the
// named subscription (falling back to the agent's owner); without one, an
// agent notifies its own owner and a member notifies themself.
func (h *Handler) CreateOwnerNotice(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if workspaceID == "" {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return
	}
	var req CreateOwnerNoticeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	userID := requestUserID(r)
	actorType, actorID := h.resolveActor(r, userID, workspaceID)
	in := agentrouter.NoticeInput{
		WorkspaceID: workspaceID, Title: req.Title, Body: req.Body, Urgency: req.Urgency,
		DedupeKey: req.DedupeKey, SenderType: actorType, SenderID: actorID,
	}
	agentID := ""
	if actorType == "agent" {
		agentID = actorID
		if task, ok := h.taskFromRequestHeader(r); ok {
			in.SourceTaskID = uuidToString(task.ID)
		}
	}
	if ref := strings.TrimSpace(req.SubscriptionID); ref != "" {
		id, ok := parseUUIDOrBadRequest(w, ref, "subscription_id")
		if !ok {
			return
		}
		sub, err := h.routerStore().Get(r.Context(), workspaceID, uuidToString(id))
		if err != nil {
			h.routerFail(w, r, err)
			return
		}
		if actorType == "agent" && sub.AgentID != actorID {
			writeError(w, http.StatusForbidden, "an agent can only notify through its own subscriptions")
			return
		}
		in.SourceSubscriptionID, agentID = sub.ID, sub.AgentID
		if sub.OwnerUserID != nil {
			in.RecipientUserID = *sub.OwnerUserID
		}
	}
	if in.RecipientUserID == "" {
		if agentID == "" {
			in.RecipientUserID = actorID
		} else {
			agent, ok := h.loadAgentForUser(w, r, agentID)
			if !ok {
				return
			}
			in.RecipientUserID = uuidToString(agent.OwnerID)
		}
	}
	if in.RecipientUserID == "" {
		writeError(w, http.StatusUnprocessableEntity, "no owner to notify")
		return
	}
	for _, ref := range req.Refs {
		issue, ok := h.loadIssueForUser(w, r, strings.TrimSpace(ref))
		if !ok {
			return
		}
		in.Refs = append(in.Refs, uuidToString(issue.ID))
	}
	if err := in.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	nf := h.routerNotifier()
	if actorType == "agent" {
		n, err := nf.CountRecentNotices(r.Context(), workspaceID, actorID)
		if err != nil {
			h.routerFail(w, r, err)
			return
		}
		if n >= maxAgentNoticesPerHour {
			writeError(w, http.StatusTooManyRequests, "this agent sent "+strconv.Itoa(n)+" notices in the last hour; the limit is "+strconv.Itoa(maxAgentNoticesPerHour))
			return
		}
	}
	notice, created, err := nf.Send(r.Context(), in)
	if err != nil {
		h.routerFail(w, r, err)
		return
	}
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"notice": notice, "created": created})
}

// ListOwnerNotices lists notices: a member's own (addressed to them), or an
// agent's sent ones; subscription_id narrows to one subscription.
func (h *Handler) ListOwnerNotices(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if workspaceID == "" {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return
	}
	actorType, actorID := h.resolveActor(r, requestUserID(r), workspaceID)
	f := agentrouter.NoticeFilter{WorkspaceID: workspaceID}
	if actorType == "agent" {
		f.SenderID = actorID
	} else {
		f.RecipientUserID = actorID
	}
	if v := strings.TrimSpace(r.URL.Query().Get("subscription_id")); v != "" {
		if _, ok := parseUUIDOrBadRequest(w, v, "subscription_id"); !ok {
			return
		}
		f.SubscriptionID = v
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 200")
			return
		}
		f.Limit = n
	}
	out, err := h.routerNotifier().ListNotices(r.Context(), f)
	if err != nil {
		h.routerFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"notices": out})
}
