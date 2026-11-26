// Added by the mouse9527/multica fork: cross-workspace read-only access (M-307).

package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/crossread"
	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/middleware"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// maxCrossReadGrantNote caps a grant's note, as the column's check does.
const maxCrossReadGrantNote = 500

// MountCrossWorkspaceReadGrants mounts /api/workspaces/{id}/cross-read-grants
// inside the owner/admin group: the target workspace's owner or admin lets the
// agents of another workspace read this one, revokes that, and reads the
// audit trail. People only; every route answers 404 while
// cross_workspace_read is off.
func (h *Handler) MountCrossWorkspaceReadGrants(r chi.Router) {
	r.Use(h.requireCrossWorkspaceRead)
	r.Use(RequireHumanActor)
	r.Get("/", h.ListCrossWorkspaceReadGrants)
	r.Post("/", h.CreateCrossWorkspaceReadGrant)
	r.Get("/audit", h.ListCrossWorkspaceReadAudit)
	r.Delete("/{grantId}", h.RevokeCrossWorkspaceReadGrant)
}

// isCrossWorkspaceReader reports whether the request is served under a
// cross-workspace read grant. X-Actor-Source is server-set only.
func isCrossWorkspaceReader(r *http.Request) bool {
	return r.Header.Get("X-Actor-Source") == middleware.ActorSourceCrossWorkspaceReader
}

// isChatAttachment reports whether an attachment outside any issue belongs to
// chat: a chat session or message (IM channels included), or a chat run's
// upload, which only chat tasks may make (UploadFile's task_id gate) and
// which is bound to the run's reply when it completes.
func isChatAttachment(att db.Attachment) bool {
	return att.ChatSessionID.Valid || att.ChatMessageID.Valid || att.TaskID.Valid
}

func (h *Handler) crossReadStore() *crossread.Store {
	return crossread.NewStore(h.DB, h.FeatureFlags)
}

func (h *Handler) requireCrossWorkspaceRead(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.DB == nil || !h.crossReadStore().Enabled(r.Context()) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// CreateCrossWorkspaceReadGrantRequest is the body of a new grant. AgentID
// empty means every agent of the source workspace; ExpiresAt empty means
// until revoked.
type CreateCrossWorkspaceReadGrantRequest struct {
	SourceWorkspaceID string `json:"source_workspace_id"`
	AgentID           string `json:"agent_id"`
	ExpiresAt         string `json:"expires_at"`
	Note              string `json:"note"`
}

func (h *Handler) ListCrossWorkspaceReadGrants(w http.ResponseWriter, r *http.Request) {
	target, ok := parseUUIDOrBadRequest(w, workspaceIDFromURL(r, "id"), "workspace id")
	if !ok {
		return
	}
	grants, err := h.crossReadStore().ListGrants(r.Context(), uuidToString(target))
	if err != nil {
		slog.Warn("list cross-workspace read grants failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to list grants")
		return
	}
	writeJSON(w, http.StatusOK, grants)
}

func (h *Handler) CreateCrossWorkspaceReadGrant(w http.ResponseWriter, r *http.Request) {
	target, ok := parseUUIDOrBadRequest(w, workspaceIDFromURL(r, "id"), "workspace id")
	if !ok {
		return
	}
	var req CreateCrossWorkspaceReadGrantRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	source, ok := parseUUIDOrBadRequest(w, strings.TrimSpace(req.SourceWorkspaceID), "source_workspace_id")
	if !ok {
		return
	}
	if source == target {
		writeError(w, http.StatusBadRequest, "source_workspace_id must be another workspace")
		return
	}
	if _, err := h.Queries.GetWorkspace(r.Context(), source); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusBadRequest, "source workspace not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to load source workspace")
		return
	}
	agentID := ""
	if s := strings.TrimSpace(req.AgentID); s != "" {
		agent, ok := parseUUIDOrBadRequest(w, s, "agent_id")
		if !ok {
			return
		}
		if _, err := h.Queries.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{ID: agent, WorkspaceID: source}); err != nil {
			if isNotFound(err) {
				writeError(w, http.StatusBadRequest, "agent not found in the source workspace")
				return
			}
			writeError(w, http.StatusInternalServerError, "failed to load agent")
			return
		}
		agentID = uuidToString(agent)
	}
	var expiresAt *time.Time
	if s := strings.TrimSpace(req.ExpiresAt); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			writeError(w, http.StatusBadRequest, "expires_at must be an RFC 3339 time")
			return
		}
		if !t.After(time.Now()) {
			writeError(w, http.StatusBadRequest, "expires_at must be in the future")
			return
		}
		expiresAt = &t
	}
	note := strings.TrimSpace(req.Note)
	if len([]rune(note)) > maxCrossReadGrantNote {
		writeError(w, http.StatusBadRequest, "note is too long")
		return
	}
	grant, err := h.crossReadStore().CreateGrant(r.Context(), crossread.CreateGrantParams{
		TargetWorkspaceID: uuidToString(target),
		SourceWorkspaceID: uuidToString(source),
		AgentID:           agentID,
		Note:              note,
		CreatedBy:         requestUserID(r),
		ExpiresAt:         expiresAt,
	})
	if err != nil {
		slog.Warn("create cross-workspace read grant failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to create grant")
		return
	}
	slog.Info("cross-workspace read grant created",
		"grant_id", grant.ID, "target_workspace_id", grant.TargetWorkspaceID,
		"source_workspace_id", grant.SourceWorkspaceID, "agent_id", grant.AgentID, "created_by", grant.CreatedBy)
	writeJSON(w, http.StatusCreated, grant)
}

func (h *Handler) RevokeCrossWorkspaceReadGrant(w http.ResponseWriter, r *http.Request) {
	target, ok := parseUUIDOrBadRequest(w, workspaceIDFromURL(r, "id"), "workspace id")
	if !ok {
		return
	}
	grantID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "grantId"), "grant id")
	if !ok {
		return
	}
	grant, found, err := h.crossReadStore().RevokeGrant(r.Context(), uuidToString(target), uuidToString(grantID), requestUserID(r))
	if err != nil {
		slog.Warn("revoke cross-workspace read grant failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to revoke grant")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "grant not found")
		return
	}
	slog.Info("cross-workspace read grant revoked",
		"grant_id", grant.ID, "target_workspace_id", grant.TargetWorkspaceID, "revoked_by", grant.RevokedBy)
	writeJSON(w, http.StatusOK, grant)
}

func (h *Handler) ListCrossWorkspaceReadAudit(w http.ResponseWriter, r *http.Request) {
	target, ok := parseUUIDOrBadRequest(w, workspaceIDFromURL(r, "id"), "workspace id")
	if !ok {
		return
	}
	limit := crossread.MaxAuditPage
	if s := r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = n
	}
	rows, err := h.crossReadStore().ListAudit(r.Context(), uuidToString(target), limit)
	if err != nil {
		slog.Warn("list cross-workspace read audit failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to list audit")
		return
	}
	writeJSON(w, http.StatusOK, rows)
}
