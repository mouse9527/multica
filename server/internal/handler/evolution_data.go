// Added by the mouse9527/multica fork: Evolution P2, configuration and the
// signal channel (M-136); P3 shares the run gate with proposals (M-137); the
// metrics source (M-296).

package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/evolution"
	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/projectaccess"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type evolutionDatabase struct {
	dbExecutor
	txStarter
}

func (h *Handler) evolutionDB() evolution.Database {
	return evolutionDatabase{h.DB, h.TxStarter}
}

// evolutionRunResponse is one analysis run as the settings page shows it.
type evolutionRunResponse struct {
	ID            string     `json:"id"`
	Kind          string     `json:"kind"`
	Status        string     `json:"status"`
	IssueID       *string    `json:"issue_id"`
	WindowUntil   time.Time  `json:"window_until"`
	ServedItems   int        `json:"served_items"`
	FailureReason string     `json:"failure_reason"`
	CreatedAt     time.Time  `json:"created_at"`
	FinishedAt    *time.Time `json:"finished_at"`
}

type evolutionConfigResponse struct {
	evolution.State
	Limits evolution.Limits       `json:"limits"`
	Runs   []evolutionRunResponse `json:"runs"`
}

type putEvolutionConfigRequest struct {
	Config                  *evolution.Config `json:"config"`
	AcknowledgeMemberNotice bool              `json:"acknowledge_member_notice"`
}

// GetEvolutionConfig returns the workspace's Evolution configuration, what is
// being read, and the latest runs. Any member may read it: members can see
// what Evolution reads.
func (h *Handler) GetEvolutionConfig(w http.ResponseWriter, r *http.Request) {
	workspaceID := ctxWorkspaceID(r.Context())
	if workspaceID == "" {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return
	}
	st, err := evolution.LoadState(r.Context(), h.evolutionDB(), workspaceID)
	if err != nil {
		h.evolutionFail(w, r, err)
		return
	}
	h.writeEvolutionConfig(w, r, workspaceID, st, http.StatusOK)
}

// PutEvolutionConfig replaces the workspace's Evolution configuration.
// Workspace owners and admins only. Turning chat reading on needs
// acknowledge_member_notice and notifies every member before any chat is
// read.
func (h *Handler) PutEvolutionConfig(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	workspaceID := ctxWorkspaceID(r.Context())
	if workspaceID == "" {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return
	}
	if actorType, _ := h.resolveActor(r, userID, workspaceID); actorType != "member" {
		writeError(w, http.StatusForbidden, "only members can configure Evolution")
		return
	}
	member, ok := h.workspaceMember(w, r, workspaceID)
	if !ok {
		return
	}
	if !roleAllowed(member.Role, "owner", "admin") {
		writeError(w, http.StatusForbidden, "only owners and admins can configure Evolution")
		return
	}
	var req putEvolutionConfigRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.Config == nil {
		writeError(w, http.StatusBadRequest, "invalid request body: send {\"config\": {...}}")
		return
	}
	st, err := evolution.SaveConfig(r.Context(), h.evolutionDB(), h.Queries, h.Bus, evolution.SaveRequest{
		WorkspaceID:             workspaceID,
		Config:                  *req.Config,
		AcknowledgeMemberNotice: req.AcknowledgeMemberNotice,
		Actor:                   evolution.Actor{Type: "member", ID: userID},
		Now:                     time.Now(),
	})
	if err != nil {
		h.evolutionFail(w, r, err)
		return
	}
	slog.Info("evolution config saved", append(logger.RequestAttrs(r), "workspace_id", workspaceID,
		"enabled", st.Config.Enabled, "comments", st.CommentsActive, "chat", st.ChatActive)...)
	h.writeEvolutionConfig(w, r, workspaceID, st, http.StatusOK)
}

func (h *Handler) writeEvolutionConfig(w http.ResponseWriter, r *http.Request, workspaceID string, st evolution.State, status int) {
	runs, err := evolution.RecentRuns(r.Context(), h.DB, workspaceID, 10)
	if err != nil {
		h.evolutionFail(w, r, err)
		return
	}
	resp := evolutionConfigResponse{State: st, Limits: evolution.CurrentLimits(), Runs: make([]evolutionRunResponse, 0, len(runs))}
	for _, run := range runs {
		out := evolutionRunResponse{
			ID: run.ID, Kind: run.Kind, Status: run.Status, WindowUntil: run.WindowUntil,
			ServedItems: run.ServedItems, FailureReason: run.FailureReason,
			CreatedAt: run.CreatedAt, FinishedAt: run.FinishedAt,
		}
		if run.IssueID != "" {
			id := run.IssueID
			out.IssueID = &id
		}
		resp.Runs = append(resp.Runs, out)
	}
	writeJSON(w, status, resp)
}

// GetEvolutionSignals serves one page of comments or chat messages to the
// Evolution agent's analysis run. Only a task token qualifies, and only one
// whose agent is this workspace's Evolution agent and whose task is live on
// the issue of the workspace's open run. The page comes from the run's
// window past the source's cursor; private projects are hidden for every
// run (the hidden set of a viewer with no principals), as are excluded
// projects and agents and private agents' chats. Run logs and task messages
// are not a source.
func (h *Handler) GetEvolutionSignals(w http.ResponseWriter, r *http.Request) {
	workspaceID, agent, task, ok := h.evolutionRunTask(w, r, "read signals")
	if !ok {
		return
	}
	ctx := r.Context()

	q := r.URL.Query()
	source := q.Get("source")
	if source == evolution.SourceMetrics {
		h.serveEvolutionMetrics(w, r, workspaceID, agent, task)
		return
	}
	if source != evolution.SourceComment && source != evolution.SourceChat {
		writeError(w, http.StatusBadRequest, "source must be comment, chat, or metrics")
		return
	}
	req := evolution.PageRequest{
		WorkspaceID: workspaceID,
		IssueID:     uuidToString(task.IssueID),
		Source:      source,
		SelfAgentID: uuidToString(agent.ID),
	}
	if after := q.Get("after"); after != "" {
		p, err := evolution.ParseToken(after)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		req.After = &p
	}
	if limit := q.Get("limit"); limit != "" {
		n, err := strconv.Atoi(limit)
		if err != nil || n < 1 || n > evolution.MaxPageSize {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and "+strconv.Itoa(evolution.MaxPageSize))
			return
		}
		req.Limit = n
	}
	st, err := evolution.LoadState(ctx, h.DB, workspaceID)
	if err != nil {
		h.evolutionFail(w, r, err)
		return
	}
	req.Config = st.Config
	// No principals: every private project is hidden, whoever started the
	// run (M-132 decision of 2026-11-18). The same set every handler uses.
	hidden, err := projectaccess.ForPrincipals(ctx, h.Queries, agent.WorkspaceID)
	if err != nil {
		h.evolutionFail(w, r, err)
		return
	}
	req.HiddenProjects = hidden.Param()

	page, err := evolution.ServePage(ctx, h.evolutionDB(), req)
	if err != nil {
		h.evolutionFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// evolutionRunTask admits only a task token whose agent is this workspace's
// Evolution agent and whose task is live on an issue. Whether that issue is
// the open run's is checked by the caller's evolution call (ErrNoOpenRun).
func (h *Handler) evolutionRunTask(w http.ResponseWriter, r *http.Request, what string) (string, db.Agent, db.AgentTaskQueue, bool) {
	workspaceID := ctxWorkspaceID(r.Context())
	if workspaceID == "" {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return "", db.Agent{}, db.AgentTaskQueue{}, false
	}
	denied := func(msg string) (string, db.Agent, db.AgentTaskQueue, bool) {
		writeError(w, http.StatusForbidden, msg)
		return "", db.Agent{}, db.AgentTaskQueue{}, false
	}
	onlyRun := "only the Evolution agent's analysis run can " + what
	if r.Header.Get("X-Actor-Source") != "task_token" {
		return denied(onlyRun)
	}
	ctx := r.Context()
	agentID, err := util.ParseUUID(r.Header.Get("X-Agent-ID"))
	if err != nil {
		return denied(onlyRun)
	}
	agent, err := h.Queries.GetAgent(ctx, agentID)
	if err != nil || uuidToString(agent.WorkspaceID) != workspaceID ||
		!agent.SystemKey.Valid || agent.SystemKey.String != service.EvolutionSystemKey {
		return denied(onlyRun)
	}
	taskID, err := util.ParseUUID(r.Header.Get("X-Task-ID"))
	if err != nil {
		return denied(onlyRun)
	}
	task, err := h.Queries.GetAgentTask(ctx, taskID)
	if err != nil || task.AgentID != agent.ID || !task.IssueID.Valid ||
		(task.Status != "dispatched" && task.Status != "running") {
		return denied("only a live task of the Evolution agent can " + what)
	}
	return workspaceID, agent, task, true
}

func (h *Handler) evolutionFail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, evolution.ErrNoticeRequired):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, evolution.ErrNoOpenRun), errors.Is(err, evolution.ErrSourceOff):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, evolution.ErrWorkspaceNotFound), errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "workspace not found")
	case evolution.IsInvalidConfig(err):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		if writeQueryCancellation(w, r, err) {
			return
		}
		slog.Warn("evolution request failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "evolution request failed")
	}
}
