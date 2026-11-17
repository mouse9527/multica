// Modified for the mouse9527/multica fork: Router inbox S3a (M-118).
//
// HTTP surface of inbox resolve: the workspace setting, a live count of the
// owner inbox against the issues that wait for the owner, and this server
// process's counters. The logic is in internal/agentrouter/inbox_resolve.go.

package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/multica-ai/multica/server/internal/agentrouter"
	"github.com/multica-ai/multica/server/internal/issuestatus"
)

type inboxResolveResponse struct {
	// Enabled is whether the server process that answered runs inbox
	// resolve (router_inbox_resolve on at its start).
	Enabled bool                            `json:"enabled"`
	Config  *agentrouter.InboxResolveConfig `json:"config"`
	Survey  *agentrouter.InboxSurvey        `json:"survey"`
	Stats   *agentrouter.InboxResolveStats  `json:"stats"`
}

func (h *Handler) inboxResolveState(r *http.Request, workspaceID string) (inboxResolveResponse, error) {
	var out inboxResolveResponse
	if s, ok := agentrouter.CurrentInboxResolveStats(); ok {
		out.Enabled, out.Stats = true, &s
	}
	cfg, err := agentrouter.LoadInboxResolveConfig(r.Context(), h.DB, workspaceID)
	if err != nil || !cfg.Active() {
		return out, err
	}
	out.Config = &cfg
	survey, err := agentrouter.SurveyInbox(r.Context(), h.DB, workspaceID, cfg)
	if err != nil {
		return out, err
	}
	out.Survey = &survey
	return out, nil
}

// GetInboxResolve returns the workspace's setting, the live counts and the
// answering process's counters. Counts only; no item content.
func (h *Handler) GetInboxResolve(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if workspaceID == "" {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return
	}
	out, err := h.inboxResolveState(r, workspaceID)
	if err != nil {
		h.routerFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type setInboxResolveRequest struct {
	// OwnerUserID defaults to the caller.
	OwnerUserID     string   `json:"owner_user_id"`
	WaitingStatuses []string `json:"waiting_statuses"`
	// Off removes the setting.
	Off bool `json:"off"`
}

// SetInboxResolve stores the workspace's setting. Owners and admins only.
func (h *Handler) SetInboxResolve(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if workspaceID == "" {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return
	}
	actorType, actorID := h.resolveActor(r, requestUserID(r), workspaceID)
	if actorType != "member" {
		writeError(w, http.StatusForbidden, "only members can change the inbox resolve setting")
		return
	}
	member, err := h.getWorkspaceMember(r.Context(), actorID, workspaceID)
	if err != nil || !roleAllowed(member.Role, "owner", "admin") {
		writeError(w, http.StatusForbidden, "only owners and admins can change the inbox resolve setting")
		return
	}
	var req setInboxResolveRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var cfg *agentrouter.InboxResolveConfig
	if !req.Off {
		owner := req.OwnerUserID
		if owner == "" {
			owner = actorID
		}
		c, err := agentrouter.InboxResolveConfig{OwnerUserID: owner, WaitingStatuses: req.WaitingStatuses}.Normalize()
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if _, err := h.getWorkspaceMember(r.Context(), c.OwnerUserID, workspaceID); err != nil {
			writeError(w, http.StatusBadRequest, "owner_user_id is not a member of this workspace")
			return
		}
		for _, status := range c.WaitingStatuses {
			if _, err := issuestatus.Resolve(r.Context(), h.Queries, parseUUID(workspaceID), status); err != nil {
				if errors.Is(err, issuestatus.ErrUnknownStatus) {
					writeError(w, http.StatusBadRequest, "unknown issue status: "+status)
					return
				}
				h.routerFail(w, r, err)
				return
			}
		}
		cfg = &c
	}
	if err := agentrouter.SetInboxResolveConfig(r.Context(), h.DB, workspaceID, cfg); err != nil {
		h.routerFail(w, r, err)
		return
	}
	out, err := h.inboxResolveState(r, workspaceID)
	if err != nil {
		h.routerFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
