// Modified for the mouse9527/multica fork: Router inbox S3b (M-133).
//
// HTTP surface of the inbox rules: the workspace's rule list, this server
// process's verdict counts, and the report that sets verdicts against what
// the owner did. The logic is in internal/agentrouter/inbox_rules.go.

package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/multica-ai/multica/server/internal/agentrouter"
	"github.com/multica-ai/multica/server/internal/issuestatus"
)

type inboxRulesResponse struct {
	// Enabled is whether the server process that answered classifies
	// (router_inbox_rules on, with the shadow hold, at its start).
	Enabled bool                          `json:"enabled"`
	Config  *agentrouter.InboxRulesConfig `json:"config"`
	Stats   *agentrouter.InboxRulesStats  `json:"stats"`
}

func (h *Handler) inboxRulesState(r *http.Request, workspaceID string) (inboxRulesResponse, error) {
	var out inboxRulesResponse
	out.Stats, out.Enabled = agentrouter.CurrentInboxRulesStats()
	cfg, err := agentrouter.LoadInboxRulesConfig(r.Context(), h.DB, workspaceID)
	if err != nil || !cfg.Active() {
		return out, err
	}
	out.Config = &cfg
	return out, nil
}

// GetInboxRules returns the workspace's rule list and the answering
// process's counts.
func (h *Handler) GetInboxRules(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if workspaceID == "" {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return
	}
	out, err := h.inboxRulesState(r, workspaceID)
	if err != nil {
		h.routerFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type setInboxRulesRequest struct {
	Rules []agentrouter.InboxRule `json:"rules"`
	// Defaults stores agentrouter.DefaultInboxRules with WaitingStatuses
	// instead of Rules.
	Defaults        bool     `json:"defaults"`
	WaitingStatuses []string `json:"waiting_statuses"`
	// Off removes the rule list.
	Off bool `json:"off"`
}

// SetInboxRules stores the workspace's rule list. Owners and admins only.
func (h *Handler) SetInboxRules(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if workspaceID == "" {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return
	}
	actorType, actorID := h.resolveActor(r, requestUserID(r), workspaceID)
	if actorType != "member" {
		writeError(w, http.StatusForbidden, "only members can change the inbox rules")
		return
	}
	member, err := h.getWorkspaceMember(r.Context(), actorID, workspaceID)
	if err != nil || !roleAllowed(member.Role, "owner", "admin") {
		writeError(w, http.StatusForbidden, "only owners and admins can change the inbox rules")
		return
	}
	var req setInboxRulesRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var cfg *agentrouter.InboxRulesConfig
	if !req.Off {
		in := agentrouter.InboxRulesConfig{Rules: req.Rules}
		if req.Defaults {
			if len(req.Rules) > 0 {
				writeError(w, http.StatusBadRequest, "give rules or defaults, not both")
				return
			}
			if len(req.WaitingStatuses) == 0 {
				writeError(w, http.StatusBadRequest, "defaults need at least one waiting status")
				return
			}
			in = agentrouter.DefaultInboxRules(req.WaitingStatuses)
		}
		c, err := in.Normalize()
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if !c.Active() {
			writeError(w, http.StatusBadRequest, "give at least one rule, defaults, or off")
			return
		}
		for _, rule := range c.Rules {
			for _, status := range rule.ToStatuses {
				if _, err := issuestatus.Resolve(r.Context(), h.Queries, parseUUID(workspaceID), status); err != nil {
					if errors.Is(err, issuestatus.ErrUnknownStatus) {
						writeError(w, http.StatusBadRequest, "unknown issue status: "+status)
						return
					}
					h.routerFail(w, r, err)
					return
				}
			}
		}
		cfg = &c
	}
	if err := agentrouter.SetInboxRulesConfig(r.Context(), h.DB, workspaceID, cfg); err != nil {
		h.routerFail(w, r, err)
		return
	}
	out, err := h.inboxRulesState(r, workspaceID)
	if err != nil {
		h.routerFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// GetInboxRulesReport reports, per rule, how many classified items the owner
// acted on afterwards. Like the hold list, a member sees the items addressed
// to them and an agent the items of its subscriptions. Counts only.
func (h *Handler) GetInboxRulesReport(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if workspaceID == "" {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return
	}
	actorType, actorID := h.resolveActor(r, requestUserID(r), workspaceID)
	f := agentrouter.RulesReportFilter{WorkspaceID: workspaceID}
	if actorType == "agent" {
		f.AgentID = actorID
	} else {
		f.RecipientUserID = actorID
	}
	for name, dst := range map[string]*time.Duration{"since": &f.Since, "window": &f.Window} {
		if v := r.URL.Query().Get(name); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				writeError(w, http.StatusBadRequest, name+" must be a positive duration such as 24h")
				return
			}
			*dst = d
		}
	}
	out, err := h.routerStore().InboxRulesReport(r.Context(), f)
	if err != nil {
		h.routerFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
