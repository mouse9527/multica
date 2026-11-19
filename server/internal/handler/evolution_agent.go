// Added by the mouse9527/multica fork: the built-in Evolution agent (M-135, M-136).

package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/evolution"
	"github.com/multica-ai/multica/server/internal/logger"
	obsmetrics "github.com/multica-ai/multica/server/internal/metrics"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Evolution's product-defined identity. Server constants for the same reason
// as Mika's: the public CreateAgent API accepts neither `kind` nor
// `system_key`, so only this endpoint can mint the agent.
const (
	// One analysis at a time: later phases hand it one window per run.
	evolutionAgentMaxConcurrency = 1
	evolutionAgentVisibility     = "workspace"
	evolutionAgentPermissionMode = "public_to"
	evolutionAgentAvatarURL      = agentEmojiAvatarPrefix + "🧬"
)

// The agent is created under a localized name and description; both are
// owner-editable afterwards.
var evolutionAgentNames = map[string]string{
	"en": "Evolution",
	"zh": "进化",
	"ko": "진화",
	"ja": "進化",
}

// The descriptions also tell every member what Evolution reads once its
// scheduled analysis is on (M-136); they stay within the agent description
// limit so an owner can still edit them.
var evolutionAgentDescriptions = map[string]string{
	"en": "Proposes improvements to this workspace's agents, workflows, and team. With scheduled analysis on, it reads comments and members' chats daily (not private projects, private agents, or run logs). Every change waits for an owner's approval.",
	"zh": "为本工作区的智能体、工作流和团队提出改进提案。开启定时分析后，每天读取工作区的评论和成员的对话（不含私有项目、私有智能体和运行日志）。每项改动都要等 Owner 批准。",
	"ko": "이 워크스페이스의 에이전트, 워크플로, 팀에 대한 개선안을 제안합니다. 정기 분석을 켜면 매일 댓글과 멤버의 채팅을 읽습니다(비공개 프로젝트, 비공개 에이전트, 실행 로그 제외). 모든 변경은 오너의 승인을 기다립니다.",
	"ja": "このワークスペースのエージェント、ワークフロー、チームの改善案を提案します。定期分析をオンにすると、毎日コメントとメンバーのチャットを読みます（非公開プロジェクト、非公開エージェント、実行ログは除く）。すべての変更はオーナーの承認を待ちます。",
}

type enableEvolutionRequest struct {
	RuntimeID string `json:"runtime_id"`
	Language  string `json:"language"`
	// Model is optional: empty means the runtime's default.
	Model string `json:"model"`
}

// evolutionResponse is the workspace's Evolution agent, or null when it has
// not been enabled.
type evolutionResponse struct {
	Agent *AgentResponse `json:"agent"`
}

// MountEvolution mounts /api/evolution. Every route answers 404 while
// evolution_v1 is off.
func (h *Handler) MountEvolution(r chi.Router) {
	r.Use(h.requireEvolutionV1)
	r.Get("/", h.GetEvolution)
	r.With(RequireHumanActor).Post("/enable", h.EnableEvolution)
	// P2 (M-136): configuration, and the analysis run's signal channel.
	r.Get("/config", h.GetEvolutionConfig)
	r.With(RequireHumanActor).Put("/config", h.PutEvolutionConfig)
	r.Get("/signals", h.GetEvolutionSignals)
}

func (h *Handler) requireEvolutionV1(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !evolution.Enabled(r.Context(), h.FeatureFlags) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// GetEvolution returns the workspace's Evolution agent. Any member may read
// it; the settings page uses it to decide between "enable" and "enabled".
func (h *Handler) GetEvolution(w http.ResponseWriter, r *http.Request) {
	workspaceID := ctxWorkspaceID(r.Context())
	if workspaceID == "" {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return
	}
	agent, err := h.Queries.GetAgentBySystemKey(r.Context(), db.GetAgentBySystemKeyParams{
		WorkspaceID: parseUUID(workspaceID),
		SystemKey:   pgtype.Text{String: service.EvolutionSystemKey, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, evolutionResponse{})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to look up the workspace agent")
		return
	}
	h.writeEvolutionAgent(w, r, agent, http.StatusOK)
}

// EnableEvolution provisions the workspace's Evolution agent on the given
// runtime, or returns the existing one. Workspace owners and admins only.
//
// Idempotency is by system_key inside the workspace, under a per-workspace
// advisory lock, for the reasons spelled out on CreateMikaAgent.
func (h *Handler) EnableEvolution(w http.ResponseWriter, r *http.Request) {
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
		writeError(w, http.StatusForbidden, "only members can enable Evolution")
		return
	}
	member, ok := h.workspaceMember(w, r, workspaceID)
	if !ok {
		return
	}
	if !roleAllowed(member.Role, "owner", "admin") {
		writeError(w, http.StatusForbidden, "only owners and admins can enable Evolution")
		return
	}

	var req enableEvolutionRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	name, ok := evolutionAgentNames[req.Language]
	if !ok {
		writeError(w, http.StatusBadRequest, "language must be en, zh, ko, or ja")
		return
	}
	runtimeID, ok := parseUUIDOrBadRequest(w, req.RuntimeID, "runtime_id")
	if !ok {
		return
	}
	wsUUID := parseUUID(workspaceID)

	if existing, err := h.Queries.GetAgentBySystemKey(r.Context(), db.GetAgentBySystemKeyParams{
		WorkspaceID: wsUUID,
		SystemKey:   pgtype.Text{String: service.EvolutionSystemKey, Valid: true},
	}); err == nil {
		h.writeEvolutionAgent(w, r, existing, http.StatusOK)
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "failed to look up the workspace agent")
		return
	}

	runtime, err := h.getAgentRuntime(r.Context(), obsmetrics.RuntimeLookupSourceRuntimeAPI, runtimeID)
	if err != nil || uuidToString(runtime.WorkspaceID) != workspaceID {
		writeError(w, http.StatusBadRequest, "runtime not found in this workspace")
		return
	}
	if !canUseRuntimeForAgent(member, runtime) {
		writeError(w, http.StatusForbidden, "you cannot bind an agent to this runtime")
		return
	}

	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to start agent create transaction")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)

	if _, err := tx.Exec(r.Context(),
		"SELECT pg_advisory_xact_lock(hashtextextended($1, 0))",
		"evolution:"+workspaceID,
	); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to lock the workspace agent")
		return
	}
	// Re-check under the lock: a concurrent enable may have committed.
	if existing, err := qtx.GetAgentBySystemKey(r.Context(), db.GetAgentBySystemKeyParams{
		WorkspaceID: wsUUID,
		SystemKey:   pgtype.Text{String: service.EvolutionSystemKey, Valid: true},
	}); err == nil {
		// End the transaction, and with it the lock, before building the
		// response: that reads through the pool, and holding a pooled
		// connection while waiting for another one deadlocks once concurrent
		// enables fill the pool.
		tx.Rollback(r.Context())
		h.writeEvolutionAgent(w, r, existing, http.StatusOK)
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "failed to look up the workspace agent")
		return
	}

	// Same runtime revocation protocol as CreateMikaAgent.
	if _, err := qtx.LockWorkspaceForChatSessionCreate(r.Context(), wsUUID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "workspace not found")
		} else {
			writeError(w, http.StatusInternalServerError, "failed to lock workspace")
		}
		return
	}
	runtime, err = qtx.LockAgentRuntimeForAccessChange(r.Context(), runtimeID)
	if err != nil || runtime.WorkspaceID != wsUUID {
		writeError(w, http.StatusBadRequest, "runtime not found in this workspace")
		return
	}
	if !canUseRuntimeForAgent(member, runtime) {
		writeError(w, http.StatusForbidden, "you cannot bind an agent to this runtime")
		return
	}

	// Agent names are unique per workspace (including archived agents). Check
	// first so a taken name is a clear 409 rather than an aborted insert.
	var nameTaken bool
	if err := tx.QueryRow(r.Context(),
		`SELECT EXISTS (SELECT 1 FROM agent WHERE workspace_id = $1 AND name = $2)`,
		wsUUID, name,
	).Scan(&nameTaken); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to check the agent name")
		return
	}
	if nameTaken {
		writeError(w, http.StatusConflict, fmt.Sprintf("an agent named %q already exists in this workspace; rename it and try again", name))
		return
	}

	model := strings.TrimSpace(req.Model)
	created, err := qtx.CreateSystemUserAgent(r.Context(), db.CreateSystemUserAgentParams{
		WorkspaceID:        wsUUID,
		Name:               name,
		Description:        evolutionAgentDescriptions[req.Language],
		AvatarUrl:          pgtype.Text{String: evolutionAgentAvatarURL, Valid: true},
		RuntimeMode:        runtime.RuntimeMode,
		RuntimeID:          runtime.ID,
		Model:              pgtype.Text{String: model, Valid: model != ""},
		Visibility:         evolutionAgentVisibility,
		PermissionMode:     evolutionAgentPermissionMode,
		MaxConcurrentTasks: evolutionAgentMaxConcurrency,
		OwnerID:            parseUUID(userID),
		SystemKey:          pgtype.Text{String: service.EvolutionSystemKey, Valid: true},
	})
	if err != nil {
		slog.Warn("create evolution agent failed", append(logger.RequestAttrs(r), "error", err, "workspace_id", workspaceID)...)
		writeError(w, http.StatusInternalServerError, "failed to create the workspace agent")
		return
	}
	// Workspace-invocable: every member may chat with it and assign it work.
	if err := replaceInvocationTargetsWithQueries(r.Context(), qtx, created.ID, parseUUID(userID), []targetSpec{
		{targetType: invocationTargetWorkspace, targetID: wsUUID},
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save agent access")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to commit agent create")
		return
	}
	slog.Info("evolution agent created", append(logger.RequestAttrs(r), "agent_id", uuidToString(created.ID), "workspace_id", workspaceID)...)

	if runtime.Status == "online" {
		h.TaskService.ReconcileAgentStatus(r.Context(), created.ID)
		created, _ = h.Queries.GetAgent(r.Context(), created.ID)
	}
	resp := h.evolutionAgentResponse(r, created)
	h.publish(protocol.EventAgentCreated, workspaceID, "member", userID, map[string]any{"agent": broadcastAgentResponse(resp)})
	writeJSON(w, http.StatusCreated, evolutionResponse{Agent: &resp})
}

func (h *Handler) writeEvolutionAgent(w http.ResponseWriter, r *http.Request, agent db.Agent, status int) {
	resp := h.evolutionAgentResponse(r, agent)
	writeJSON(w, status, evolutionResponse{Agent: &resp})
}

func (h *Handler) evolutionAgentResponse(r *http.Request, agent db.Agent) AgentResponse {
	resp := h.agentToResponse(agent)
	if err := h.enrichAgentResponseWithTargets(r.Context(), &resp, agent.ID); err != nil {
		slog.Warn("evolution agent: load invocation targets failed", append(logger.RequestAttrs(r), "error", err, "agent_id", uuidToString(agent.ID))...)
	}
	return resp
}
