// Added by the mouse9527/multica fork: Evolution P4, apply and rollback
// (M-138).

package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/evolution"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type evolutionVersionResponse struct {
	ID         string    `json:"id"`
	Version    int       `json:"version"`
	Action     string    `json:"action"`
	Field      string    `json:"field"`
	RevertedOf *string   `json:"reverted_of"`
	ActorID    string    `json:"actor_id"`
	CreatedAt  time.Time `json:"created_at"`
}

// evolutionProposalResponse is a proposal as the issue page shows it: what
// it changes, where it stands, and what the caller may do with it.
type evolutionProposalResponse struct {
	IssueID    string                   `json:"issue_id"`
	TargetType string                   `json:"target_type"`
	TargetID   string                   `json:"target_id"`
	TargetName string                   `json:"target_name"`
	Field      string                   `json:"field"`
	Change     evolution.ProposalChange `json:"change"`
	// State is pending, applied, or rolled_back.
	State string `json:"state"`
	// Applicable is false for proposals people carry out by hand (squads).
	Applicable bool `json:"applicable"`
	// Stale: the target changed (or is gone) since the proposal was written,
	// so it can no longer be applied.
	Stale bool `json:"stale"`
	// ChangedSinceApply: the target changed after the apply, so a rollback
	// would overwrite that change and is refused.
	ChangedSinceApply bool                       `json:"changed_since_apply"`
	CanApply          bool                       `json:"can_apply"`
	CanRollback       bool                       `json:"can_rollback"`
	Versions          []evolutionVersionResponse `json:"versions"`
}

const (
	proposalStatePending    = "pending"
	proposalStateApplied    = "applied"
	proposalStateRolledBack = "rolled_back"
)

func applyRefused(code, format string, args ...any) error {
	return &evolution.RejectedError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// evolutionHumanAdmin reports whether the caller is a person (not an agent
// or a machine credential) who owns or administers the workspace: the only
// callers that may apply or roll back a proposal.
func (h *Handler) evolutionHumanAdmin(r *http.Request, workspaceID string) (db.Member, bool) {
	if isMachineCredentialActor(r) {
		return db.Member{}, false
	}
	userID := requestUserID(r)
	if userID == "" {
		return db.Member{}, false
	}
	if actorType, _ := h.resolveActor(r, userID, workspaceID); actorType != "member" {
		return db.Member{}, false
	}
	member, ok := ctxMember(r.Context())
	if !ok {
		m, err := h.Queries.GetMemberByUserAndWorkspace(r.Context(), db.GetMemberByUserAndWorkspaceParams{
			UserID: parseUUID(userID), WorkspaceID: parseUUID(workspaceID),
		})
		if err != nil {
			return db.Member{}, false
		}
		member = m
	}
	return member, roleAllowed(member.Role, "owner", "admin")
}

// loadEvolutionProposal resolves the {issueId} of a proposal route through
// the issue loader, so a proposal moved into a private project the caller
// cannot see is not found, as the issue itself is not.
func (h *Handler) loadEvolutionProposal(w http.ResponseWriter, r *http.Request) (evolution.ProposalRecord, bool) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "issueId"))
	if !ok {
		return evolution.ProposalRecord{}, false
	}
	rec, err := evolution.GetProposalRecord(r.Context(), h.DB, uuidToString(issue.WorkspaceID), uuidToString(issue.ID))
	if errors.Is(err, evolution.ErrNotProposal) {
		writeError(w, http.StatusNotFound, err.Error())
		return evolution.ProposalRecord{}, false
	}
	if err != nil {
		h.evolutionFail(w, r, err)
		return evolution.ProposalRecord{}, false
	}
	return rec, true
}

// GetEvolutionProposal returns a proposal's state for its issue page. Any
// member may read it; can_apply and can_rollback are true only for owners
// and admins.
func (h *Handler) GetEvolutionProposal(w http.ResponseWriter, r *http.Request) {
	rec, ok := h.loadEvolutionProposal(w, r)
	if !ok {
		return
	}
	_, admin := h.evolutionHumanAdmin(r, rec.WorkspaceID)
	resp, err := h.evolutionProposalState(r.Context(), h.DB, rec, admin)
	if err != nil {
		h.evolutionFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) evolutionProposalState(ctx context.Context, q evolution.Querier, rec evolution.ProposalRecord, admin bool) (evolutionProposalResponse, error) {
	versions, err := evolution.ProposalVersions(ctx, q, rec.WorkspaceID, rec.IssueID)
	if err != nil {
		return evolutionProposalResponse{}, err
	}
	snap, _, err := evolution.ReadTarget(ctx, q, rec.WorkspaceID, rec.TargetType, rec.TargetID, false)
	missing := errors.Is(err, evolution.ErrTargetNotFound)
	if err != nil && !missing {
		return evolutionProposalResponse{}, err
	}
	hash := ""
	if !missing {
		hash = snap.ContentHash()
	}
	active, applied := evolution.ActiveApply(versions)
	resp := evolutionProposalResponse{
		IssueID: rec.IssueID, TargetType: rec.TargetType, TargetID: rec.TargetID, TargetName: snap.Name,
		Field:      rec.Field,
		Change:     evolution.ProposalChange{Before: rec.ChangeBefore, After: rec.ChangeAfter},
		State:      proposalStatePending,
		Applicable: evolution.CheckApplyField(rec.TargetType, rec.Field) == nil,
		Versions:   make([]evolutionVersionResponse, 0, len(versions)),
	}
	switch {
	case applied:
		resp.State = proposalStateApplied
	case len(versions) > 0:
		resp.State = proposalStateRolledBack
	}
	resp.Stale = !applied && (missing || hash != rec.BaseHash)
	resp.ChangedSinceApply = applied && (missing || hash != active.AfterHash)
	resp.CanApply = admin && resp.Applicable && !applied && !resp.Stale
	resp.CanRollback = admin && applied && !resp.ChangedSinceApply
	for _, v := range versions {
		out := evolutionVersionResponse{
			ID: v.ID, Version: v.Version, Action: v.Action, Field: v.Field, ActorID: v.ActorID, CreatedAt: v.CreatedAt,
		}
		if v.RevertedOf != "" {
			id := v.RevertedOf
			out.RevertedOf = &id
		}
		resp.Versions = append(resp.Versions, out)
	}
	return resp, nil
}

// ApplyEvolutionProposal writes an approved proposal's change to its agent
// or workflow and appends a version row. Workspace owners and admins only;
// agents never (the route also carries RequireHumanActor). When the target
// changed since the proposal was written, nothing is written: the answer is
// 409 and the proposal is marked stale.
func (h *Handler) ApplyEvolutionProposal(w http.ResponseWriter, r *http.Request) {
	h.changeEvolutionProposal(w, r, false)
}

// RollbackEvolutionProposal restores what the proposal's apply replaced and
// appends a version row pointing at that apply. It is refused when the
// target changed after the apply, so a later edit is never overwritten.
func (h *Handler) RollbackEvolutionProposal(w http.ResponseWriter, r *http.Request) {
	h.changeEvolutionProposal(w, r, true)
}

func (h *Handler) changeEvolutionProposal(w http.ResponseWriter, r *http.Request, rollback bool) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	workspaceID := ctxWorkspaceID(r.Context())
	if workspaceID == "" {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return
	}
	member, ok := h.evolutionHumanAdmin(r, workspaceID)
	if !ok {
		writeError(w, http.StatusForbidden, "only workspace owners and admins can apply or roll back Evolution proposals")
		return
	}
	rec, ok := h.loadEvolutionProposal(w, r)
	if !ok {
		return
	}
	if err := evolution.CheckApplyField(rec.TargetType, rec.Field); err != nil {
		h.writeEvolutionChangeError(w, r, err)
		return
	}
	ctx := r.Context()
	agentName := h.evolutionAgentName(ctx, workspaceID)

	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		h.evolutionFail(w, r, err)
		return
	}
	defer tx.Rollback(ctx)
	qtx := h.Queries.WithTx(tx)
	wsUUID := parseUUID(workspaceID)
	if rec.TargetType == evolution.TargetWorkflow {
		// The lock UpdateIssueWorkflow takes first: workflow edits and status
		// archives wait for this write, and it for them.
		if err := qtx.LockIssueStatusCatalog(ctx, wsUUID); err != nil {
			h.evolutionFail(w, r, err)
			return
		}
	}
	// The proposal issue before the target: two changes of one proposal run
	// one after the other. An issue path that then enqueues takes the same
	// order (issue, then agent); the enqueue order (agent, then FOR KEY SHARE
	// on the issue) does not conflict with this lock.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM issue WHERE id = $1::uuid AND workspace_id = $2::uuid FOR NO KEY UPDATE`,
		rec.IssueID, workspaceID); err != nil {
		h.evolutionFail(w, r, err)
		return
	}
	snap, st, err := evolution.ReadTarget(ctx, tx, workspaceID, rec.TargetType, rec.TargetID, true)
	if errors.Is(err, evolution.ErrTargetNotFound) {
		h.writeEvolutionChangeError(w, r, applyRefused(evolution.ApplyFailed, "the %s this proposal changes no longer exists", rec.TargetType))
		return
	}
	if err != nil {
		h.evolutionFail(w, r, err)
		return
	}
	if err := evolution.RefuseTarget(rec.TargetType, rec.TargetID, st); err != nil {
		h.writeEvolutionChangeError(w, r, err)
		return
	}
	versions, err := evolution.ProposalVersions(ctx, tx, workspaceID, rec.IssueID)
	if err != nil {
		h.evolutionFail(w, r, err)
		return
	}
	active, applied := evolution.ActiveApply(versions)

	note := evolution.ProposalNote{
		WorkspaceID: workspaceID, IssueID: rec.IssueID, ActorID: userID, AgentName: agentName,
		TargetType: rec.TargetType, TargetName: snap.Name, Field: rec.Field,
	}
	var want string
	if rollback {
		if !applied {
			h.writeEvolutionChangeError(w, r, applyRefused(evolution.ApplyNotApplied, "this proposal is not applied, so there is nothing to roll back"))
			return
		}
		if snap.ContentHash() != active.AfterHash {
			h.writeEvolutionChangeError(w, r, applyRefused(evolution.ApplyChangedSinceApply,
				"the %s changed after this proposal was applied; rolling back would overwrite that change, so change it by hand", rec.TargetType))
			return
		}
		want = active.Before[active.Field]
	} else {
		if applied {
			h.writeEvolutionChangeError(w, r, applyRefused(evolution.ApplyAlreadyApplied, "this proposal is already applied (version %d)", active.Version))
			return
		}
		if snap.ContentHash() != rec.BaseHash {
			tx.Rollback(ctx)
			h.markEvolutionProposalStale(ctx, note)
			h.writeEvolutionChangeError(w, r, applyRefused(evolution.ApplyStale,
				"the %s changed after this proposal was written; nothing was applied, and the proposal is marked out of date", rec.TargetType))
			return
		}
		want = rec.ProposedValue
	}

	published, err := h.writeEvolutionField(ctx, r, qtx, tx, member, rec, want)
	if err != nil {
		h.writeEvolutionChangeError(w, r, err)
		return
	}
	after, _, err := evolution.ReadTarget(ctx, tx, workspaceID, rec.TargetType, rec.TargetID, false)
	if err != nil {
		h.evolutionFail(w, r, err)
		return
	}
	if !evolution.OnlyFieldChanged(snap, after, rec.Field, want) || (rollback && after.ContentHash() != active.BeforeHash) {
		// The rows read above are locked, so only a skill binding added by
		// someone else meanwhile can make skills differ: the target changed,
		// and nothing is written.
		if rec.Field == "skills" {
			tx.Rollback(ctx)
			if rollback {
				h.writeEvolutionChangeError(w, r, applyRefused(evolution.ApplyChangedSinceApply,
					"the agent's skills changed while rolling back; nothing was changed"))
				return
			}
			h.markEvolutionProposalStale(ctx, note)
			h.writeEvolutionChangeError(w, r, applyRefused(evolution.ApplyStale,
				"the agent's skills changed while applying; nothing was applied, and the proposal is marked out of date"))
			return
		}
		h.writeEvolutionChangeError(w, r, applyRefused(evolution.ApplyFailed,
			"the %s would not end up as the proposal describes (its %s is normalized differently); nothing was changed, so change it by hand", rec.TargetType, rec.Field))
		return
	}
	version := evolution.Version{
		WorkspaceID: workspaceID, TargetType: rec.TargetType, TargetID: rec.TargetID,
		Action: evolution.ActionApply, Field: rec.Field, ProposalIssueID: rec.IssueID,
		BeforeHash: snap.ContentHash(), AfterHash: after.ContentHash(), Before: snap.Fields, After: after.Fields,
		ActorID: userID,
	}
	state := evolution.StateApplied
	if rollback {
		version.Action, version.RevertedOf = evolution.ActionRollback, active.ID
		state = evolution.StateRolledBack
		note.RevertedVersion = active.Version
	}
	version, err = evolution.InsertVersion(ctx, tx, version)
	if err != nil {
		h.evolutionFail(w, r, err)
		return
	}
	if _, err := evolution.SetProposalState(ctx, tx, workspaceID, rec.IssueID, state); err != nil {
		h.evolutionFail(w, r, err)
		return
	}
	note.Version = version.Version
	comment, err := evolution.AddProposalNote(ctx, qtx, note, state)
	if err != nil {
		h.evolutionFail(w, r, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		h.evolutionFail(w, r, err)
		return
	}
	evolution.PublishComment(h.Bus, workspaceID, comment)
	published()
	slog.Info("evolution proposal "+version.Action, append(logger.RequestAttrs(r), "workspace_id", workspaceID,
		"issue_id", rec.IssueID, "target_type", rec.TargetType, "target_id", rec.TargetID, "field", rec.Field,
		"version", version.Version)...)

	resp, err := h.evolutionProposalState(ctx, h.DB, rec, true)
	if err != nil {
		h.evolutionFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// markEvolutionProposalStale records, once, that a proposal no longer
// applies: the state on its issue and a comment saying why.
func (h *Handler) markEvolutionProposalStale(ctx context.Context, note evolution.ProposalNote) {
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		slog.Warn("evolution: mark proposal stale", "issue_id", note.IssueID, "error", err)
		return
	}
	defer tx.Rollback(ctx)
	changed, err := evolution.SetProposalState(ctx, tx, note.WorkspaceID, note.IssueID, evolution.StateStale)
	if err != nil || !changed {
		if err != nil {
			slog.Warn("evolution: mark proposal stale", "issue_id", note.IssueID, "error", err)
		}
		return
	}
	comment, err := evolution.AddProposalNote(ctx, h.Queries.WithTx(tx), note, evolution.StateStale)
	if err != nil {
		slog.Warn("evolution: mark proposal stale", "issue_id", note.IssueID, "error", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		slog.Warn("evolution: mark proposal stale", "issue_id", note.IssueID, "error", err)
		return
	}
	evolution.PublishComment(h.Bus, note.WorkspaceID, comment)
}

// writeEvolutionField writes one whitelisted field through the queries the
// agent and workflow endpoints use, with their checks. It returns what to
// publish once the transaction commits.
func (h *Handler) writeEvolutionField(ctx context.Context, r *http.Request, qtx *db.Queries, tx evolution.Querier, member db.Member, rec evolution.ProposalRecord, value string) (func(), error) {
	workspaceID := rec.WorkspaceID
	actorID := uuidToString(member.UserID)
	switch rec.TargetType {
	case evolution.TargetAgent:
		agentID := parseUUID(rec.TargetID)
		var err error
		switch rec.Field {
		case "instructions":
			_, err = qtx.UpdateAgent(ctx, db.UpdateAgentParams{ID: agentID, Instructions: pgtype.Text{String: value, Valid: true}})
		case "description":
			if utf8.RuneCountInString(value) > maxAgentDescriptionLength {
				return nil, applyRefused(evolution.ApplyFailed, "description must be %d characters or fewer", maxAgentDescriptionLength)
			}
			_, err = qtx.UpdateAgent(ctx, db.UpdateAgentParams{ID: agentID, Description: pgtype.Text{String: value, Valid: true}})
		case "skills":
			err = h.writeEvolutionSkills(ctx, r, qtx, tx, workspaceID, agentID, value)
		default:
			return nil, evolution.CheckApplyField(rec.TargetType, rec.Field)
		}
		if err != nil {
			return nil, err
		}
		return func() { h.publishEvolutionAgent(r, workspaceID, actorID, agentID) }, nil
	case evolution.TargetWorkflow:
		if err := h.writeEvolutionWorkflow(ctx, r, qtx, workspaceID, rec, value); err != nil {
			return nil, err
		}
		return func() { h.publishIssueWorkflowChanged(workspaceID, member, "updated") }, nil
	}
	return nil, evolution.CheckApplyField(rec.TargetType, rec.Field)
}

// writeEvolutionSkills makes the agent's enabled skills the named ones:
// bindings to other skills are removed, missing ones added, disabled ones
// re-enabled. Disabled bindings outside the list are left alone; the
// snapshot does not show them.
func (h *Handler) writeEvolutionSkills(ctx context.Context, r *http.Request, qtx *db.Queries, tx evolution.Querier, workspaceID string, agentID pgtype.UUID, value string) error {
	names := evolution.SkillNames(value)
	want := map[string]bool{}
	if len(names) > 0 {
		rows, err := tx.Query(ctx, `SELECT name, id::text FROM skill WHERE workspace_id = $1::uuid AND name = ANY($2::text[])`,
			workspaceID, names)
		if err != nil {
			return err
		}
		ids := map[string]string{}
		for rows.Next() {
			var name, id string
			if err := rows.Scan(&name, &id); err != nil {
				rows.Close()
				return err
			}
			ids[name] = id
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, name := range names {
			id, ok := ids[name]
			if !ok {
				return applyRefused(evolution.ApplyFailed, "skill %q does not exist in this workspace", name)
			}
			want[id] = true
		}
		agent, err := qtx.GetAgent(ctx, agentID)
		if err != nil {
			return err
		}
		if provider, ok := h.resolveAgentProvider(r, agent.WorkspaceID, agent.RuntimeID); ok && provider == "qoder_cloud" {
			return applyRefused(evolution.ApplyFailed, "%v", qoderOverrides{skills: len(want)}.validate())
		}
	}
	rows, err := tx.Query(ctx, `SELECT skill_id::text, enabled FROM agent_skill WHERE agent_id = $1`, agentID)
	if err != nil {
		return err
	}
	bound := map[string]bool{}
	for rows.Next() {
		var id string
		var enabled bool
		if err := rows.Scan(&id, &enabled); err != nil {
			rows.Close()
			return err
		}
		bound[id] = enabled
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for id, enabled := range bound {
		if enabled && !want[id] {
			if err := qtx.RemoveAgentSkill(ctx, db.RemoveAgentSkillParams{AgentID: agentID, SkillID: parseUUID(id)}); err != nil {
				return err
			}
		}
	}
	for id := range want {
		enabled, ok := bound[id]
		switch {
		case !ok:
			if err := qtx.AddAgentSkill(ctx, db.AddAgentSkillParams{AgentID: agentID, SkillID: parseUUID(id)}); err != nil {
				return err
			}
		case !enabled:
			if _, err := qtx.SetAgentSkillEnabled(ctx, db.SetAgentSkillEnabledParams{AgentID: agentID, SkillID: parseUUID(id), Enabled: true}); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeEvolutionWorkflow writes one field of a workflow with the checks of
// UpdateIssueWorkflow. A change that would leave issues on a status it
// removes is refused: moving them needs a person to choose where.
func (h *Handler) writeEvolutionWorkflow(ctx context.Context, r *http.Request, qtx *db.Queries, workspaceID string, rec evolution.ProposalRecord, value string) error {
	wsUUID := parseUUID(workspaceID)
	current, err := qtx.GetIssueWorkflow(ctx, db.GetIssueWorkflowParams{ID: parseUUID(rec.TargetID), WorkspaceID: wsUUID})
	if err != nil {
		return err
	}
	currentSteps, err := issueworkflow.DecodeSteps(current.Steps)
	if err != nil {
		return err
	}
	name, description, steps := current.Name, current.Description, currentSteps
	switch rec.Field {
	case "name":
		if current.IsDefault {
			return applyRefused(evolution.ApplyFailed, "the Default workflow's name cannot be changed")
		}
		name = value
	case "description":
		description = value
	case "steps":
		steps, err = issueworkflow.DecodeSteps([]byte(value))
		if err != nil {
			return applyRefused(evolution.ApplyFailed, "the proposed steps are not valid: %v", err)
		}
	default:
		return evolution.CheckApplyField(rec.TargetType, rec.Field)
	}
	name, description, steps = issueworkflow.Normalize(name, description, steps)
	catalog, err := h.workflowStatusCatalog(ctx, qtx, wsUUID)
	if err != nil {
		return err
	}
	if err := issueworkflow.Validate(name, description, steps, current.IsDefault, catalog); err != nil {
		return applyRefused(evolution.ApplyFailed, "%s", err.Error())
	}
	if status, msg := h.validateHandoffHandlers(ctx, r, workspaceID, steps, currentSteps); status != 0 {
		return applyRefused(evolution.ApplyFailed, "%s", msg)
	}
	if !current.IsDefault {
		projects, err := qtx.ListProjectsUsingIssueWorkflow(ctx, db.ListProjectsUsingIssueWorkflowParams{WorkspaceID: wsUUID, WorkflowID: current.ID})
		if err != nil {
			return err
		}
		if len(projects) > 0 {
			projectIDs := make([]pgtype.UUID, len(projects))
			for i, p := range projects {
				projectIDs[i] = p.ID
			}
			counts, err := qtx.CountProjectIssuesByStatus(ctx, db.CountProjectIssuesByStatusParams{WorkspaceID: wsUUID, ProjectIds: projectIDs})
			if err != nil {
				return err
			}
			next := issueworkflow.Definition{ID: current.ID, Name: name, Steps: steps}
			if plan := mappingPlan(counts, next.Has, func(string) string { return "" }); len(plan.Required) > 0 {
				return applyRefused(evolution.ApplyFailed,
					"%d issue(s) are on statuses this change removes; make the change by hand in the workflow settings, which asks where to move them", plan.AffectedIssues)
			}
		}
	}
	raw, err := issueworkflow.EncodeSteps(steps)
	if err != nil {
		return err
	}
	if _, err := qtx.UpdateIssueWorkflow(ctx, db.UpdateIssueWorkflowParams{
		ID:               current.ID,
		WorkspaceID:      wsUUID,
		Name:             name,
		Description:      description,
		InitialStatusKey: issueworkflow.StartStatus,
		Steps:            raw,
	}); err != nil {
		if isUniqueViolation(err) {
			return applyRefused(evolution.ApplyFailed, "a workflow with this name already exists")
		}
		return err
	}
	return nil
}

// publishEvolutionAgent announces an agent Evolution changed, the way
// UpdateAgent does.
func (h *Handler) publishEvolutionAgent(r *http.Request, workspaceID, actorID string, agentID pgtype.UUID) {
	ctx := r.Context()
	agent, err := h.Queries.GetAgent(ctx, agentID)
	if err != nil {
		slog.Warn("evolution: publish agent", "agent_id", uuidToString(agentID), "error", err)
		return
	}
	resp := h.agentToResponse(agent)
	if err := h.enrichAgentResponseWithTargets(ctx, &resp, agent.ID); err != nil {
		slog.Warn("evolution: publish agent", "agent_id", uuidToString(agentID), "error", err)
		return
	}
	if err := h.attachAgentSkills(ctx, &resp, agent.ID); err != nil {
		slog.Warn("evolution: publish agent", "agent_id", uuidToString(agentID), "error", err)
		return
	}
	h.publish(protocol.EventAgentStatus, workspaceID, "member", actorID, map[string]any{"agent": broadcastAgentResponse(resp)})
}

// evolutionAgentName is the workspace's Evolution agent's name, which picks
// the language of the notes on proposal issues.
func (h *Handler) evolutionAgentName(ctx context.Context, workspaceID string) string {
	agent, err := h.Queries.GetAgentBySystemKey(ctx, db.GetAgentBySystemKeyParams{
		WorkspaceID: parseUUID(workspaceID),
		SystemKey:   pgtype.Text{String: service.EvolutionSystemKey, Valid: true},
	})
	if err != nil {
		return ""
	}
	return agent.Name
}

func (h *Handler) writeEvolutionChangeError(w http.ResponseWriter, r *http.Request, err error) {
	var rejected *evolution.RejectedError
	if !errors.As(err, &rejected) {
		h.evolutionFail(w, r, err)
		return
	}
	status := http.StatusUnprocessableEntity
	switch rejected.Code {
	case evolution.ApplyStale, evolution.ApplyAlreadyApplied, evolution.ApplyNotApplied, evolution.ApplyChangedSinceApply:
		status = http.StatusConflict
	}
	slog.Info("evolution proposal change refused", append(logger.RequestAttrs(r), "code", rejected.Code, "reason", rejected.Message)...)
	writeJSON(w, status, rejected)
}
