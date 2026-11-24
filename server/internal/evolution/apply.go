// Added by the mouse9527/multica fork: Evolution P4, apply and rollback
// (M-138); version reads shared with P5 (M-139).

package evolution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/multica-ai/multica/server/internal/events"
	inboxwriter "github.com/multica-ai/multica/server/internal/inbox"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// ApplyFields is the field whitelist apply and rollback write, per target
// type. Squad proposals are not here: they are text for a person to carry
// out by hand, as are proposals to create or merge agents.
var ApplyFields = map[string][]string{
	TargetAgent:    {"instructions", "description", "skills"},
	TargetWorkflow: {"steps", "name", "description"},
}

// Codes of a refused apply or rollback, carried by *RejectedError.
const (
	ApplyStale             = "proposal_stale"
	ApplyAlreadyApplied    = "already_applied"
	ApplyNotApplied        = "not_applied"
	ApplyChangedSinceApply = "changed_since_apply"
	ApplyFailed            = "cannot_apply"
)

// ErrNotProposal means the issue is not a proposal apply knows: not in the
// workspace, or filed without a stored change.
var ErrNotProposal = errors.New("this issue is not an Evolution proposal that can be applied")

// Proposal states, kept in the proposal issue's evolution_state key for
// people and filters to see. Apply and rollback never read it: they read
// the version rows.
const (
	MetaState = "evolution_state"

	StateApplied    = "applied"
	StateRolledBack = "rolled_back"
	StateStale      = "stale"
)

// Version actions.
const (
	ActionApply    = "apply"
	ActionRollback = "rollback"
)

// CheckApplyField refuses a target type or field apply never writes.
func CheckApplyField(targetType, field string) error {
	fields, ok := ApplyFields[targetType]
	if !ok {
		return reject(RejectTarget, "%s proposals are carried out by hand; nothing applies them", targetType)
	}
	if forbiddenFields[strings.ToLower(field)] {
		return reject(RejectField, "field %q is never written: permissions, secrets, environment, MCP configuration, runtimes, and visibility are out of bounds", field)
	}
	for _, f := range fields {
		if f == field {
			return nil
		}
	}
	return reject(RejectField, "apply writes only %s of a %s", strings.Join(fields, ", "), targetType)
}

// RefuseTarget refuses targets nothing may change: Evolution itself, hidden
// system agents, and archived agents.
func RefuseTarget(targetType, targetID string, st TargetState) error {
	return refuseTarget(targetType, targetID, st)
}

// whitespaceRun matches what strings.Fields splits on.
const whitespaceRun = `[\t\n\v\f\r \x{85}\p{Z}]+`

// PatchText applies a change to a field's text and returns the result and
// how many places change.before matched. before is found the way
// CheckTarget finds it, ignoring how whitespace is laid out; the one match
// becomes after. An empty before appends after on a line of its own. The
// result means something only when the count is 1.
func PatchText(current, before, after string) (string, int) {
	if before == "" {
		switch {
		case current == "":
			return after, 1
		case strings.HasSuffix(current, "\n"):
			return current + after, 1
		default:
			return current + "\n" + after, 1
		}
	}
	tokens := strings.Fields(before)
	for i, tok := range tokens {
		tokens[i] = regexp.QuoteMeta(tok)
	}
	matches := regexp.MustCompile(strings.Join(tokens, whitespaceRun)).FindAllStringIndex(current, -1)
	if len(matches) != 1 {
		return "", len(matches)
	}
	m := matches[0]
	return current[:m[0]] + after + current[m[1]:], 1
}

// SameFieldValue reports whether a field read back holds want. Skills
// compare as a set of names and steps as JSON values; other fields compare
// exactly.
func SameFieldValue(field, got, want string) bool {
	switch field {
	case "skills":
		a, b := SkillNames(got), SkillNames(want)
		sort.Strings(a)
		sort.Strings(b)
		return reflect.DeepEqual(dedupe(a), dedupe(b))
	case "steps":
		var a, b any
		if json.Unmarshal([]byte(got), &a) != nil || json.Unmarshal([]byte(want), &b) != nil {
			return false
		}
		return reflect.DeepEqual(a, b)
	}
	return got == want
}

func dedupe(sorted []string) []string {
	out := []string{}
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// OnlyFieldChanged reports whether after differs from before in field alone,
// and field holds want.
func OnlyFieldChanged(before, after TargetSnapshot, field, want string) bool {
	if !SameFieldValue(field, after.Fields[field], want) {
		return false
	}
	for k, v := range before.Fields {
		if k != field && after.Fields[k] != v {
			return false
		}
	}
	return len(before.Fields) == len(after.Fields)
}

// ProposalRecord is what a proposal changes, stored when it is filed. Apply
// reads the change from here, never from the issue body or metadata.
type ProposalRecord struct {
	IssueID       string
	WorkspaceID   string
	RunID         string
	TargetType    string
	TargetID      string
	Field         string
	BaseHash      string
	ChangeBefore  string
	ChangeAfter   string
	BaseValue     string
	ProposedValue string
	CreatedAt     time.Time
}

func saveProposalRecord(ctx context.Context, q Querier, r ProposalRecord) error {
	_, err := q.Exec(ctx, `
		INSERT INTO evolution_proposal (issue_id, workspace_id, run_id, target_type, target_id, field, base_hash,
		                                change_before, change_after, base_value, proposed_value)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5::uuid, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (issue_id) DO NOTHING`,
		r.IssueID, r.WorkspaceID, r.RunID, r.TargetType, r.TargetID, r.Field, r.BaseHash,
		r.ChangeBefore, r.ChangeAfter, r.BaseValue, r.ProposedValue)
	return err
}

// GetProposalRecord returns the stored change of a proposal issue that still
// exists in the workspace.
func GetProposalRecord(ctx context.Context, q Querier, workspaceID, issueID string) (ProposalRecord, error) {
	var r ProposalRecord
	err := q.QueryRow(ctx, `
		SELECT p.issue_id::text, p.workspace_id::text, p.run_id::text, p.target_type, p.target_id::text, p.field,
		       p.base_hash, p.change_before, p.change_after, p.base_value, p.proposed_value, p.created_at
		FROM evolution_proposal p
		JOIN issue i ON i.id = p.issue_id AND i.workspace_id = p.workspace_id
		WHERE p.issue_id = $1::uuid AND p.workspace_id = $2::uuid`, issueID, workspaceID).
		Scan(&r.IssueID, &r.WorkspaceID, &r.RunID, &r.TargetType, &r.TargetID, &r.Field,
			&r.BaseHash, &r.ChangeBefore, &r.ChangeAfter, &r.BaseValue, &r.ProposedValue, &r.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotProposal
	}
	return r, err
}

// Version is one row of a target's version history.
type Version struct {
	ID              string
	WorkspaceID     string
	TargetType      string
	TargetID        string
	Version         int
	Action          string
	Field           string
	ProposalIssueID string
	RevertedOf      string
	BeforeHash      string
	AfterHash       string
	Before          map[string]string
	After           map[string]string
	ActorID         string
	CreatedAt       time.Time
}

// ProposalVersions lists the version rows a proposal wrote, oldest first.
func ProposalVersions(ctx context.Context, q Querier, workspaceID, issueID string) ([]Version, error) {
	return queryVersions(ctx, q, `
		WHERE workspace_id = $1::uuid AND proposal_issue_id = $2::uuid
		ORDER BY version, created_at`, workspaceID, issueID)
}

// queryVersions reads version rows; where is the query's WHERE clause and
// everything after it.
func queryVersions(ctx context.Context, q Querier, where string, args ...any) ([]Version, error) {
	rows, err := q.Query(ctx, `
		SELECT id::text, workspace_id::text, target_type, target_id::text, version, action, field,
		       proposal_issue_id::text, COALESCE(reverted_of::text, ''), before_hash, after_hash,
		       before_snapshot, after_snapshot, actor_id::text, created_at
		FROM evolution_config_version `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Version{}
	for rows.Next() {
		var v Version
		var before, after []byte
		if err := rows.Scan(&v.ID, &v.WorkspaceID, &v.TargetType, &v.TargetID, &v.Version, &v.Action, &v.Field,
			&v.ProposalIssueID, &v.RevertedOf, &v.BeforeHash, &v.AfterHash, &before, &after, &v.ActorID, &v.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(before, &v.Before); err != nil {
			return nil, fmt.Errorf("version %s: %w", v.ID, err)
		}
		if err := json.Unmarshal(after, &v.After); err != nil {
			return nil, fmt.Errorf("version %s: %w", v.ID, err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ActiveApply returns the proposal's apply that no rollback has undone.
func ActiveApply(versions []Version) (Version, bool) {
	reverted := map[string]bool{}
	for _, v := range versions {
		if v.Action == ActionRollback {
			reverted[v.RevertedOf] = true
		}
	}
	for i := len(versions) - 1; i >= 0; i-- {
		if v := versions[i]; v.Action == ActionApply && !reverted[v.ID] {
			return v, true
		}
	}
	return Version{}, false
}

// InsertVersion appends a version row; its number is the target's next. The
// caller holds the target's row lock, so numbers do not race.
func InsertVersion(ctx context.Context, q Querier, v Version) (Version, error) {
	before, err := json.Marshal(v.Before)
	if err != nil {
		return v, err
	}
	after, err := json.Marshal(v.After)
	if err != nil {
		return v, err
	}
	var revertedOf *string
	if v.RevertedOf != "" {
		revertedOf = &v.RevertedOf
	}
	err = q.QueryRow(ctx, `
		INSERT INTO evolution_config_version (workspace_id, target_type, target_id, version, action, field,
		                                      proposal_issue_id, reverted_of, before_hash, after_hash,
		                                      before_snapshot, after_snapshot, actor_id)
		SELECT $1::uuid, $2, $3::uuid,
		       COALESCE((SELECT max(version) FROM evolution_config_version
		                 WHERE workspace_id = $1::uuid AND target_type = $2 AND target_id = $3::uuid), 0) + 1,
		       $4, $5, $6::uuid, $7::uuid, $8, $9, $10::jsonb, $11::jsonb, $12::uuid
		RETURNING id::text, version, created_at`,
		v.WorkspaceID, v.TargetType, v.TargetID, v.Action, v.Field, v.ProposalIssueID, revertedOf,
		v.BeforeHash, v.AfterHash, string(before), string(after), v.ActorID).
		Scan(&v.ID, &v.Version, &v.CreatedAt)
	return v, err
}

// SetProposalState records a proposal's state on its issue. It reports
// whether the state changed.
func SetProposalState(ctx context.Context, q Querier, workspaceID, issueID, state string) (bool, error) {
	meta, _ := json.Marshal(map[string]string{MetaState: state})
	tag, err := q.Exec(ctx, `
		UPDATE issue SET metadata = metadata || $3::jsonb, revision = revision + 1, updated_at = now()
		WHERE id = $1::uuid AND workspace_id = $2::uuid AND metadata ? 'evolution_fingerprint'
		  AND NOT (metadata @> $3::jsonb)`, issueID, workspaceID, string(meta))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ProposalNote is a comment apply or rollback leaves on the proposal issue,
// as the person who acted.
type ProposalNote struct {
	WorkspaceID string
	IssueID     string
	ActorID     string
	// AgentName is the Evolution agent's name; it picks the language.
	AgentName  string
	TargetType string
	TargetName string
	Field      string
	Version    int
	// RevertedVersion is the version a rollback undid.
	RevertedVersion int
}

type noteCopy struct {
	applied, rolledBack, stale string
	targets                    map[string]string
}

var noteCopies = map[string]noteCopy{
	"en": {
		applied:    "Applied by an owner or admin: the %s **%s** now has version %d of `%s`. While nobody changes the %[1]s, **Roll back** on this issue undoes it.",
		rolledBack: "Rolled back by an owner or admin: `%[3]s` of the %[1]s **%[2]s** is back to what it was before version %[4]d (recorded as version %[5]d).",
		stale:      "Not applied: the %s **%s** changed after this proposal was written, so it no longer applies to what is there now. Evolution can propose it again against the current version.",
		targets:    map[string]string{TargetAgent: "agent", TargetWorkflow: "workflow"},
	},
	"zh": {
		applied:    "已由所有者或管理员应用：%s **%s** 的 `%[4]s` 现为第 %[3]d 版。在没人改动该%[1]s之前，可在本 issue 上点「回滚」撤销。",
		rolledBack: "已由所有者或管理员回滚：%[1]s **%[2]s** 的 `%[3]s` 已恢复到第 %[4]d 版之前的内容（记为第 %[5]d 版）。",
		stale:      "未应用：提案之后%s **%s** 被改过，这条改动已不适用于现在的内容。进化可以基于当前版本重新提案。",
		targets:    map[string]string{TargetAgent: "智能体", TargetWorkflow: "工作流"},
	},
}

func (n ProposalNote) render(kind string) string {
	c := noteCopies[proposalLanguage(n.AgentName)]
	target := c.targets[n.TargetType]
	switch kind {
	case StateApplied:
		return fmt.Sprintf(c.applied, target, n.TargetName, n.Version, n.Field)
	case StateRolledBack:
		return fmt.Sprintf(c.rolledBack, target, n.TargetName, n.Field, n.RevertedVersion, n.Version)
	default:
		return fmt.Sprintf(c.stale, target, n.TargetName)
	}
}

// AddProposalNote writes the comment for kind (a proposal state) inside the
// caller's transaction; publish the returned row after commit with
// PublishComment.
func AddProposalNote(ctx context.Context, queries *db.Queries, n ProposalNote, kind string) (db.CreateCommentRow, error) {
	return addComment(ctx, queries, n.WorkspaceID, n.IssueID, "member", n.ActorID, n.render(kind))
}

func addComment(ctx context.Context, queries *db.Queries, workspaceID, issueID, authorType, authorID, content string) (db.CreateCommentRow, error) {
	return queries.CreateComment(ctx, db.CreateCommentParams{
		ID:          dbid.NewV7(),
		IssueID:     util.MustParseUUID(issueID),
		WorkspaceID: util.MustParseUUID(workspaceID),
		AuthorType:  authorType,
		AuthorID:    util.MustParseUUID(authorID),
		Content:     content,
		Type:        "comment",
	})
}

// PublishComment announces a comment written by this package.
func PublishComment(bus inboxwriter.Publisher, workspaceID string, created db.CreateCommentRow) {
	if bus == nil {
		return
	}
	comment := created.Comment()
	bus.Publish(events.Event{
		Type:        protocol.EventCommentCreated,
		WorkspaceID: workspaceID,
		ActorType:   comment.AuthorType,
		ActorID:     util.UUIDToString(comment.AuthorID),
		Payload: map[string]any{
			"comment": map[string]any{
				"id":          util.UUIDToString(comment.ID),
				"issue_id":    util.UUIDToString(comment.IssueID),
				"author_type": comment.AuthorType,
				"author_id":   util.UUIDToString(comment.AuthorID),
				"content":     comment.Content,
				"type":        comment.Type,
				"revision":    comment.Revision,
			},
			"issue_revision": created.IssueRevision,
		},
	})
}
