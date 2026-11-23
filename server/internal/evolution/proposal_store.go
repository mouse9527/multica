// Added by the mouse9527/multica fork: Evolution P3, filing proposals (M-137);
// P4 stores what a proposal changes (M-138); P6 files condense and team
// proposals (M-140).

package evolution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	inboxwriter "github.com/multica-ai/multica/server/internal/inbox"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ProposalLabel is the issue label every proposal carries.
const (
	ProposalLabel      = "evolution"
	proposalLabelColor = "#8b5cf6"
)

// Proposal metadata keys: flat string keys on the new proposal issue only
// (M-132 decision of 2026-11-18). Existing issues are never written to.
const (
	MetaFingerprint = "evolution_fingerprint"
	MetaTargetType  = "evolution_target_type"
	MetaTargetID    = "evolution_target_id"
	MetaField       = "evolution_field"
	MetaBaseHash    = "evolution_base_hash"
	MetaRisk        = "evolution_risk"
	MetaRunID       = "evolution_run_id"
)

// Proposal outcomes.
const (
	OutcomeCreated   = "created"
	OutcomeMerged    = "merged"
	OutcomeDuplicate = "duplicate"
)

// FileRequest is one proposal from an analysis run.
type FileRequest struct {
	WorkspaceID string
	AgentID     string
	AgentName   string
	// IssueID is the analysis or review issue of the calling task; it
	// selects the run.
	IssueID  string
	Proposal ProposalRequest
	Config   Config
	// HiddenProjects is projectaccess.Hidden.Param() with no principals: the
	// same set the signal channel hides.
	HiddenProjects []pgtype.UUID
	// CommentsActive and ChatActive are State's: a source that is not being
	// read cannot be cited (chats only after members were told).
	CommentsActive bool
	ChatActive     bool
	Now            time.Time
}

// FileResult says what happened to a proposal.
type FileResult struct {
	Outcome         string `json:"outcome"`
	IssueID         string `json:"issue_id"`
	IssueIdentifier string `json:"issue_identifier"`
	Fingerprint     string `json:"fingerprint"`
	// NewEvidence is how many evidence items were added to an existing
	// proposal (merged), or cited by the new one (created).
	NewEvidence int `json:"new_evidence"`
	// ProposalsLeft is how many more new proposals this run may open.
	ProposalsLeft int `json:"proposals_left"`
}

// Proposals files proposals: it validates them, finds the same proposal
// again, and opens a new proposal issue only when there is none. It never
// changes the target.
type Proposals struct {
	DB      Database
	Queries *db.Queries
	Issues  IssueCreator
	Bus     inboxwriter.Publisher
}

// File validates and files one proposal. A *RejectedError says why it was
// refused; ErrNoOpenRun means the task is not an open run's or review's.
//
// An analysis run files proposals that cite evidence. A condense review
// files proposals that make a listed agent's instructions shorter; they cite
// none, and the review's measurement stands in for it.
func (s *Proposals) File(ctx context.Context, req FileRequest) (FileResult, error) {
	p := &req.Proposal
	run, review, err := runOrReviewForIssue(ctx, s.DB, req.WorkspaceID, req.IssueID)
	if err != nil {
		return FileResult{}, err
	}
	var measure *CondenseCandidate
	if review != nil {
		if err := p.ValidateCondense(); err != nil {
			return FileResult{}, err
		}
		c, ok := review.Candidate(p.Target.ID)
		if !ok {
			return FileResult{}, reject(RejectTarget, "agent %s is not listed on this condense review", p.Target.ID)
		}
		// The agent may have been made private since the review listed it.
		visible, err := agentVisibleToWorkspace(ctx, s.DB, req.WorkspaceID, p.Target.ID)
		if err != nil {
			return FileResult{}, err
		}
		if !visible {
			return FileResult{}, reject(RejectTarget, "agent %s is no longer visible to the whole workspace; a condense review proposes only for agents every member can see", p.Target.ID)
		}
		measure = &c
	} else if err := p.Validate(); err != nil {
		return FileResult{}, err
	}
	if p.Target.Type == TargetAgent && p.Target.ID == strings.ToLower(req.AgentID) {
		return FileResult{}, reject(RejectTarget, "Evolution never proposes changes to itself")
	}
	snap, err := s.loadTarget(ctx, req)
	if err != nil {
		return FileResult{}, err
	}
	if err := p.CheckTarget(snap); err != nil {
		return FileResult{}, err
	}
	proposed, err := p.Patched(snap)
	if err != nil {
		return FileResult{}, err
	}
	if measure != nil && utf8.RuneCountInString(proposed) >= utf8.RuneCountInString(snap.Fields[p.Field]) {
		return FileResult{}, reject(RejectNotShorter, "the change leaves the instructions %d characters long, not shorter than the current %d",
			utf8.RuneCountInString(proposed), utf8.RuneCountInString(snap.Fields[p.Field]))
	}
	evidence, err := s.resolveEvidence(ctx, req)
	if err != nil {
		return FileResult{}, err
	}
	if measure == nil {
		if err := p.CheckEvidence(evidence, req.Config.MinEvidenceCount); err != nil {
			return FileResult{}, err
		}
	}
	served, err := s.servedSources(ctx, req, run)
	if err != nil {
		return FileResult{}, err
	}
	if err := p.CheckVerbatim(append(VerbatimSources(evidence), served...)); err != nil {
		return FileResult{}, err
	}
	names, err := s.memberNames(ctx, req.WorkspaceID, snap.Name)
	if err != nil {
		return FileResult{}, err
	}
	if err := p.CheckNames(names); err != nil {
		return FileResult{}, err
	}

	fp := Fingerprint(p.Target.Type, p.Target.ID, p.Field, p.Topic)
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return FileResult{}, err
	}
	defer tx.Rollback(ctx)
	// One proposal at a time per workspace, so two runs (or two calls of one
	// run) cannot both open the same proposal.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", "evolution-proposal:"+req.WorkspaceID); err != nil {
		return FileResult{}, err
	}
	filed, err := countRunProposals(ctx, tx, req.WorkspaceID, run.ID)
	if err != nil {
		return FileResult{}, err
	}
	left := max(req.Config.MaxProposalsPerRun-filed, 0)
	res := FileResult{Fingerprint: fp, ProposalsLeft: left}

	existing, err := findProposals(ctx, tx, req.WorkspaceID, fp)
	if err != nil {
		return FileResult{}, err
	}
	for _, e := range existing {
		if e.open() {
			res.Outcome, res.IssueID, res.IssueIdentifier = OutcomeMerged, e.ID, e.Identifier
			res.NewEvidence, err = s.appendEvidence(ctx, req, run, e, evidence)
			if err != nil {
				return FileResult{}, err
			}
			if res.NewEvidence == 0 {
				res.Outcome = OutcomeDuplicate
			}
			return res, tx.Commit(ctx)
		}
	}
	cooldown := time.Duration(req.Config.RejectedCooldownDays) * 24 * time.Hour
	for _, e := range existing {
		if e.Category == "closed" && req.Now.Sub(e.UpdatedAt) < cooldown {
			return FileResult{}, reject(RejectRecentlyRejected,
				"the same proposal (%s) was declined on %s; it is not filed again before %s",
				e.Identifier, e.UpdatedAt.In(req.Config.Location()).Format(time.DateOnly),
				e.UpdatedAt.Add(cooldown).In(req.Config.Location()).Format(time.DateOnly))
		}
	}
	if left == 0 {
		return FileResult{}, reject(RejectRunLimit, "this run already opened %d proposals, the most one run may open (max_proposals_per_run)", filed)
	}

	labelID, err := s.ensureLabel(ctx, req.WorkspaceID)
	if err != nil {
		return FileResult{}, err
	}
	runIssue, err := issueIdentifier(ctx, tx, req.WorkspaceID, run.IssueID)
	if err != nil {
		return FileResult{}, err
	}
	lang := proposalLanguage(req.AgentName)
	agentID := util.MustParseUUID(req.AgentID)
	created, err := s.Issues.Create(ctx, service.IssueCreateParams{
		WorkspaceID: util.MustParseUUID(req.WorkspaceID),
		Title:       proposalTitlePrefix[lang] + " · " + p.Title,
		Description: pgtype.Text{String: proposalView{
			lang: lang, p: p, snap: snap, evidence: evidence, runIssueID: run.IssueID, runIdentifier: runIssue,
			loc: req.Config.Location(), measure: measure, proposedRunes: utf8.RuneCountInString(proposed), cfg: req.Config,
		}.render(), Valid: true},
		// Unassigned and parked: a proposal waits for an owner and starts
		// nothing.
		Status:         "backlog",
		Priority:       "none",
		CreatorType:    "agent",
		CreatorID:      agentID,
		LabelIDs:       []pgtype.UUID{util.MustParseUUID(labelID)},
		AllowDuplicate: true,
	}, service.IssueCreateOpts{ActorID: req.AgentID, AnalyticsAgentID: req.AgentID})
	if err != nil {
		return FileResult{}, fmt.Errorf("create proposal issue: %w", err)
	}
	issueID := util.UUIDToString(created.Issue.ID)
	meta, _ := json.Marshal(map[string]string{
		MetaFingerprint: fp,
		MetaTargetType:  p.Target.Type,
		MetaTargetID:    p.Target.ID,
		MetaField:       p.Field,
		MetaBaseHash:    snap.ContentHash(),
		MetaRisk:        p.Risk,
		MetaRunID:       run.ID,
	})
	if _, err := tx.Exec(ctx, `
		UPDATE issue SET metadata = metadata || $3::jsonb, revision = revision + 1, updated_at = now()
		WHERE id = $1::uuid AND workspace_id = $2::uuid`, issueID, req.WorkspaceID, string(meta)); err != nil {
		return FileResult{}, fmt.Errorf("mark proposal issue %s: %w", issueID, err)
	}
	// What apply will write (P4), out of reach of edits to the issue. A team
	// proposal is only text for a person to carry out: nothing to record.
	if p.Target.Type != TargetTeam {
		if err := saveProposalRecord(ctx, tx, ProposalRecord{
			IssueID: issueID, WorkspaceID: req.WorkspaceID, RunID: run.ID,
			TargetType: p.Target.Type, TargetID: p.Target.ID, Field: p.Field, BaseHash: snap.ContentHash(),
			ChangeBefore: p.Change.Before, ChangeAfter: p.Change.After,
			BaseValue: snap.Fields[p.Field], ProposedValue: proposed,
		}); err != nil {
			return FileResult{}, fmt.Errorf("record proposal %s: %w", issueID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return FileResult{}, fmt.Errorf("mark proposal issue %s: %w", issueID, err)
	}
	slog.Info("evolution: proposal filed", "workspace_id", req.WorkspaceID, "run_id", run.ID,
		"issue_id", issueID, "fingerprint", fp, "target_type", p.Target.Type, "field", p.Field)
	res.Outcome, res.IssueID = OutcomeCreated, issueID
	res.IssueIdentifier = identifierOf(created.Issue.Number, runIssue)
	res.NewEvidence = len(evidence)
	res.ProposalsLeft = left - 1
	return res, nil
}

// runOrReviewForIssue returns the open analysis run whose issue is issueID,
// or else the open review whose issue it is, as the run its proposals are
// filed under (review is then non-nil).
func runOrReviewForIssue(ctx context.Context, q Querier, workspaceID, issueID string) (Run, *Review, error) {
	run, err := runForIssue(ctx, q, workspaceID, issueID)
	if !errors.Is(err, ErrNoOpenRun) {
		return run, nil, err
	}
	review, err := reviewForIssue(ctx, q, workspaceID, issueID)
	if err != nil {
		return Run{}, nil, err
	}
	return review.run(), &review, nil
}

// runForIssue returns the open run whose analysis issue is issueID.
func runForIssue(ctx context.Context, q Querier, workspaceID, issueID string) (Run, error) {
	r, err := scanRun(q.QueryRow(ctx, `SELECT `+runColumns+` FROM evolution_run
		WHERE workspace_id = $1::uuid AND issue_id = $2::uuid AND status = 'open'`, workspaceID, issueID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, ErrNoOpenRun
	}
	return r, err
}

func countRunProposals(ctx context.Context, q Querier, workspaceID, runID string) (int, error) {
	filter, _ := json.Marshal(map[string]string{MetaRunID: runID})
	var n int
	err := q.QueryRow(ctx, `SELECT count(*) FROM issue WHERE workspace_id = $1::uuid AND metadata @> $2::jsonb`,
		workspaceID, string(filter)).Scan(&n)
	return n, err
}

type existingProposal struct {
	ID          string
	Identifier  string
	Description string
	Category    string
	UpdatedAt   time.Time
}

func (e existingProposal) open() bool {
	return e.Category == "unstarted" || e.Category == "started"
}

// findProposals returns the proposal issues with fingerprint fp, newest
// first, with their status category. A status missing from the catalog
// falls back to the built-in meaning of its key.
func findProposals(ctx context.Context, q Querier, workspaceID, fp string) ([]existingProposal, error) {
	filter, _ := json.Marshal(map[string]string{MetaFingerprint: fp})
	rows, err := q.Query(ctx, `
		SELECT i.id::text, w.issue_prefix || '-' || i.number::text, COALESCE(i.description, ''),
		       COALESCE(s.category, CASE i.status WHEN 'done' THEN 'done' WHEN 'cancelled' THEN 'closed' ELSE 'started' END),
		       i.updated_at
		FROM issue i
		JOIN workspace w ON w.id = i.workspace_id
		LEFT JOIN issue_status s ON s.workspace_id = i.workspace_id AND s.key = i.status
		WHERE i.workspace_id = $1::uuid AND i.metadata @> $2::jsonb
		ORDER BY i.created_at DESC`, workspaceID, string(filter))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []existingProposal{}
	for rows.Next() {
		var e existingProposal
		if err := rows.Scan(&e.ID, &e.Identifier, &e.Description, &e.Category, &e.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// appendEvidence adds the evidence the open proposal does not cite yet as a
// comment by Evolution, and returns how many items it added. Metadata is not
// touched.
func (s *Proposals) appendEvidence(ctx context.Context, req FileRequest, run Run, e existingProposal, evidence []ResolvedEvidence) (int, error) {
	cited := e.Description
	rows, err := s.DB.Query(ctx, `
		SELECT content FROM comment
		WHERE issue_id = $1::uuid AND workspace_id = $2::uuid AND author_type = 'agent' AND author_id = $3::uuid
		  AND deleted_at IS NULL`, e.ID, req.WorkspaceID, req.AgentID)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			rows.Close()
			return 0, err
		}
		cited += "\n" + c
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	fresh := []ResolvedEvidence{}
	for _, ev := range evidence {
		if !strings.Contains(cited, ev.ID) {
			fresh = append(fresh, ev)
		}
	}
	if len(fresh) == 0 {
		return 0, nil
	}
	runIssue, err := issueIdentifier(ctx, s.DB, req.WorkspaceID, run.IssueID)
	if err != nil {
		return 0, err
	}
	lang := proposalLanguage(req.AgentName)
	content := renderMoreEvidence(lang, fresh, run.IssueID, runIssue, req.Config.Location())
	created, err := addComment(ctx, s.Queries, req.WorkspaceID, e.ID, "agent", req.AgentID, content)
	if err != nil {
		return 0, fmt.Errorf("add evidence to %s: %w", e.Identifier, err)
	}
	PublishComment(s.Bus, req.WorkspaceID, created)
	return len(fresh), nil
}

// ensureLabel returns the workspace's "evolution" issue label, creating it
// the first time.
func (s *Proposals) ensureLabel(ctx context.Context, workspaceID string) (string, error) {
	find := func() (string, error) {
		var id string
		err := s.DB.QueryRow(ctx, `
			SELECT id::text FROM issue_label
			WHERE workspace_id = $1::uuid AND resource_type = 'issue' AND lower(name) = lower($2)
			ORDER BY created_at LIMIT 1`, workspaceID, ProposalLabel).Scan(&id)
		return id, err
	}
	id, err := find()
	if err == nil || !errors.Is(err, pgx.ErrNoRows) {
		return id, err
	}
	err = s.DB.QueryRow(ctx, `
		INSERT INTO issue_label (workspace_id, resource_type, name, description, color)
		VALUES ($1::uuid, 'issue', $2, $3, $4) RETURNING id::text`,
		workspaceID, ProposalLabel, "Evolution proposals", proposalLabelColor).Scan(&id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		// Someone created it meanwhile.
		return find()
	}
	return id, err
}

func issueIdentifier(ctx context.Context, q Querier, workspaceID, issueID string) (string, error) {
	var id string
	err := q.QueryRow(ctx, `
		SELECT w.issue_prefix || '-' || i.number::text FROM issue i JOIN workspace w ON w.id = i.workspace_id
		WHERE i.id = $1::uuid AND i.workspace_id = $2::uuid`, issueID, workspaceID).Scan(&id)
	return id, err
}

// identifierOf builds an identifier with the prefix of another identifier in
// the same workspace.
func identifierOf(number int32, sibling string) string {
	prefix := sibling
	if i := strings.LastIndex(sibling, "-"); i >= 0 {
		prefix = sibling[:i]
	}
	return fmt.Sprintf("%s-%d", prefix, number)
}

// loadTarget reads the target's whitelisted fields and refuses targets that
// are not allowed: missing, archived, hidden system agents, Evolution
// itself, excluded agents, and a team other than this workspace's.
func (s *Proposals) loadTarget(ctx context.Context, req FileRequest) (TargetSnapshot, error) {
	p := req.Proposal
	if p.Target.Type == TargetTeam && !strings.EqualFold(p.Target.ID, req.WorkspaceID) {
		return TargetSnapshot{}, reject(RejectTarget, "a team proposal's target.id is this workspace's id, %s", req.WorkspaceID)
	}
	snap, st, err := ReadTarget(ctx, s.DB, req.WorkspaceID, p.Target.Type, p.Target.ID, false)
	if errors.Is(err, ErrTargetNotFound) {
		return snap, reject(RejectTarget, "%s %s was not found in this workspace", p.Target.Type, p.Target.ID)
	}
	if err != nil {
		return snap, err
	}
	if err := refuseTarget(p.Target.Type, p.Target.ID, st); err != nil {
		return snap, err
	}
	if p.Target.Type == TargetAgent {
		for _, ex := range req.Config.ExcludeAgents {
			if ex == p.Target.ID {
				return snap, reject(RejectTarget, "agent %s is excluded from Evolution", p.Target.ID)
			}
		}
	}
	return snap, nil
}

// refuseTarget refuses targets nothing may propose or change: Evolution
// itself, hidden system agents, and archived agents and squads.
func refuseTarget(targetType, targetID string, st TargetState) error {
	switch {
	case targetType == TargetAgent && st.SystemKey == service.EvolutionSystemKey:
		return reject(RejectTarget, "Evolution never proposes changes to itself")
	case targetType == TargetAgent && st.Kind == "system":
		return reject(RejectTarget, "agent %s is an internal system agent and cannot be a target", targetID)
	case st.Archived:
		return reject(RejectTarget, "%s %s is archived", targetType, targetID)
	}
	return nil
}

// resolveEvidence loads every evidence item under the signal channel's
// rules: what Evolution may not read (private and excluded projects, private
// and excluded agents, its own comments and chats) cannot be cited. Content
// is redacted as the channel serves it.
func (s *Proposals) resolveEvidence(ctx context.Context, req FileRequest) ([]ResolvedEvidence, error) {
	excludedAgents := append([]string{req.AgentID}, req.Config.ExcludeAgents...)
	out := make([]ResolvedEvidence, 0, len(req.Proposal.Evidence))
	for i, e := range req.Proposal.Evidence {
		if e.Type == EvidenceChat && !req.ChatActive {
			return nil, reject(RejectEvidence, "evidence[%d]: chats are not being read in this workspace and cannot be cited", i)
		}
		if e.Type == EvidenceComment && !req.CommentsActive {
			return nil, reject(RejectEvidence, "evidence[%d]: comments are not being read in this workspace and cannot be cited", i)
		}
		r := ResolvedEvidence{ProposalEvidence: e}
		var err error
		switch e.Type {
		case EvidenceComment:
			err = s.DB.QueryRow(ctx, `
				SELECT c.created_at, c.content, c.issue_id::text, w.issue_prefix || '-' || i.number::text
				FROM comment c
				JOIN issue i ON i.id = c.issue_id AND i.workspace_id = $1::uuid
				JOIN workspace w ON w.id = i.workspace_id
				WHERE c.id = $2::uuid AND c.workspace_id = $1::uuid
				  AND c.deleted_at IS NULL AND c.type = 'comment' AND c.author_type IN ('member', 'agent')
				  AND ($3::uuid[] IS NULL OR i.project_id IS NULL OR NOT (i.project_id = ANY($3::uuid[])))
				  AND (i.project_id IS NULL OR NOT (i.project_id = ANY(COALESCE($4::uuid[], '{}'))))
				  AND NOT (c.author_type = 'agent' AND c.author_id = ANY(COALESCE($5::uuid[], '{}')))`,
				req.WorkspaceID, e.ID, req.HiddenProjects, req.Config.ExcludeProjects, excludedAgents).
				Scan(&r.CreatedAt, &r.Content, &r.IssueID, &r.IssueIdentifier)
			r.Context = "issue:" + r.IssueID
		case EvidenceChat:
			err = s.DB.QueryRow(ctx, `
				SELECT m.created_at, m.content, s.id::text
				FROM chat_message m
				JOIN chat_session s ON s.id = m.chat_session_id AND s.workspace_id = $1::uuid
				JOIN agent a ON a.id = s.agent_id AND a.workspace_id = $1::uuid
				WHERE m.id = $2::uuid AND m.message_kind = 'message'
				  AND a.permission_mode = 'public_to'
				  AND EXISTS (SELECT 1 FROM agent_invocation_target t
				              WHERE t.agent_id = a.id AND t.target_type = 'workspace')
				  AND NOT (s.agent_id = ANY(COALESCE($5::uuid[], '{}')))
				  AND ($3::uuid[] IS NULL OR s.project_id IS NULL OR NOT (s.project_id = ANY($3::uuid[])))
				  AND (s.project_id IS NULL OR NOT (s.project_id = ANY(COALESCE($4::uuid[], '{}'))))`,
				req.WorkspaceID, e.ID, req.HiddenProjects, req.Config.ExcludeProjects, excludedAgents).
				Scan(&r.CreatedAt, &r.Content, &r.ChatSessionID)
			r.Context = "chat:" + r.ChatSessionID
		case EvidenceIssue:
			// Evolution's own analysis and proposal issues are not evidence.
			err = s.DB.QueryRow(ctx, `
				SELECT i.created_at, i.id::text, w.issue_prefix || '-' || i.number::text
				FROM issue i JOIN workspace w ON w.id = i.workspace_id
				WHERE i.id = $2::uuid AND i.workspace_id = $1::uuid
				  AND ($3::uuid[] IS NULL OR i.project_id IS NULL OR NOT (i.project_id = ANY($3::uuid[])))
				  AND (i.project_id IS NULL OR NOT (i.project_id = ANY(COALESCE($4::uuid[], '{}'))))
				  AND NOT (i.metadata ? 'evolution_fingerprint')
				  AND NOT EXISTS (SELECT 1 FROM evolution_run r WHERE r.issue_id = i.id)
				  AND NOT EXISTS (SELECT 1 FROM evolution_review v WHERE v.issue_id = i.id)`,
				req.WorkspaceID, e.ID, req.HiddenProjects, req.Config.ExcludeProjects).
				Scan(&r.CreatedAt, &r.IssueID, &r.IssueIdentifier)
			r.Context = "issue:" + r.IssueID
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, reject(RejectEvidence, "evidence[%d]: %s %s was not found, or Evolution may not read it", i, e.Type, e.ID)
		}
		if err != nil {
			return nil, err
		}
		r.Content, _ = Redact(r.Content)
		out = append(out, r)
	}
	return out, nil
}

// servedSources re-reads what the signal channel served this run, so text
// the run read but does not cite cannot be pasted into a proposal either.
// Each source's range runs from its cursor (the run's start, since cursors
// move only when a run ends) to how far the run was served; with the cursor
// gone (the source was turned off meanwhile) it starts max_window_days
// before the run's upper bound, which can only include more.
func (s *Proposals) servedSources(ctx context.Context, req FileRequest, run Run) ([]VerbatimSource, error) {
	excludedAgents := append([]string{req.AgentID}, req.Config.ExcludeAgents...)
	limit := req.Config.MaxItemsPerWindow + MaxPageSize
	out := []VerbatimSource{}
	for _, source := range []string{SourceComment, SourceChat} {
		served, ok := run.Served[source]
		if !ok {
			continue
		}
		start, ok, err := GetCursor(ctx, s.DB, req.WorkspaceID, source)
		if err != nil {
			return nil, err
		}
		if !ok {
			start = Position{TS: run.WindowUntil.Add(-time.Duration(req.Config.MaxWindowDays) * 24 * time.Hour)}
		}
		var items []Signal
		if source == SourceComment {
			items, err = readComments(ctx, s.DB, req.WorkspaceID, start, served.TS, req.HiddenProjects, req.Config.ExcludeProjects, excludedAgents, limit)
		} else {
			items, err = readChats(ctx, s.DB, req.WorkspaceID, start, served.TS, req.HiddenProjects, req.Config.ExcludeProjects, excludedAgents, limit)
		}
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			content, _ := Redact(it.Content)
			out = append(out, VerbatimSource{Type: source, ID: it.ID, Content: content})
		}
	}
	return out, nil
}

// memberNames lists the workspace members' names, leaving out one equal to
// the target's own name (an agent named like a person stays nameable).
func (s *Proposals) memberNames(ctx context.Context, workspaceID, targetName string) ([]string, error) {
	rows, err := s.DB.Query(ctx, `
		SELECT u.name FROM member m JOIN "user" u ON u.id = m.user_id WHERE m.workspace_id = $1::uuid`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if !strings.EqualFold(strings.TrimSpace(name), strings.TrimSpace(targetName)) {
			out = append(out, name)
		}
	}
	return out, rows.Err()
}
