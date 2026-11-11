package githubissues

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/githubissues/issuekey"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// StatusLabelPrefix is the reserved prefix of the GitHub labels that carry a
// Multica status while an issue is open. Which label stands for which status
// is set per project in project_github_settings.status_label_map, as
// {"<status key>": "status:<name>"}.
const StatusLabelPrefix = "status:"

// GitHub's state, state_reason and labels decide a Multica status:
//
//   - closed as not_planned (or duplicate): the cancelled category, keeping
//     the current status when it is already in that category, else cancelled.
//   - closed otherwise (completed, or no reason): the done category, the same
//     way, else done.
//   - open with one mapped status label: the status the label maps to.
//   - open with no mapped status label: the project's entry status
//     (entryStatus).
//   - open with several mapped status labels: GitHub's issues list does not
//     say when each label was added. The labels whose status differs from the
//     current one were added after it; when exactly one is left, it is the
//     newest and wins. Otherwise the last of them in GitHub's label order
//     wins and a warning is logged.
//
// Labels with the prefix that the map does not know, and labels mapped to a
// done or cancelled status (only closing the issue says that), are ignored.

// githubLabel is the part of a GitHub label the status mapping reads.
type githubLabel struct {
	Name string `json:"name"`
}

// statusMapping is how one project shows its statuses on GitHub.
type statusMapping struct {
	// entry is the status of an open issue without a status label.
	entry string
	// labels maps a status key to its label as configured.
	labels map[string]string
	// keyOfLabel maps a lowercased label to its status key; GitHub label
	// names are unique without regard to case.
	keyOfLabel map[string]string
	// category resolves a status key to its lifecycle category.
	category func(key string) string
}

// loadStatusMapping reads the status mapping of the project an issue is in,
// or of the workspace when the issue is outside any project.
func loadStatusMapping(ctx context.Context, q *db.Queries, logger *slog.Logger, workspaceID, projectID pgtype.UUID) (statusMapping, error) {
	m := statusMapping{
		labels:     map[string]string{},
		keyOfLabel: map[string]string{},
		category: func(key string) string {
			return issuestatus.Category(ctx, q, workspaceID, key)
		},
	}
	var err error
	if m.entry, err = entryStatus(ctx, q, workspaceID, projectID); err != nil {
		return statusMapping{}, err
	}
	if !projectID.Valid {
		return m, nil
	}
	settings, err := q.GetProjectGithubSettings(ctx, db.GetProjectGithubSettingsParams{ProjectID: projectID, WorkspaceID: workspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return m, nil
	}
	if err != nil {
		return statusMapping{}, err
	}
	var raw map[string]string
	if len(settings.StatusLabelMap) > 0 {
		if err := json.Unmarshal(settings.StatusLabelMap, &raw); err != nil {
			logger.Warn("github issues: ignoring malformed status_label_map", "project_id", projectID, "error", err)
			return m, nil
		}
	}
	// Sorted, so which of two keys claiming one label wins does not depend
	// on map order.
	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		label := strings.TrimSpace(raw[key])
		lower := strings.ToLower(label)
		if key == "" || len(lower) <= len(StatusLabelPrefix) || !strings.HasPrefix(lower, StatusLabelPrefix) {
			logger.Warn("github issues: ignoring status label without the status: prefix", "project_id", projectID, "status", key)
			continue
		}
		if other, taken := m.keyOfLabel[lower]; taken {
			logger.Warn("github issues: status label mapped twice; keeping the first", "project_id", projectID, "label", label, "kept", other, "ignored", key)
			continue
		}
		m.labels[key] = label
		m.keyOfLabel[lower] = key
	}
	return m, nil
}

// entryStatus is the status an open GitHub issue without a status label takes:
// backlog, or the workflow's start status when the project's workflow does
// not list backlog.
func entryStatus(ctx context.Context, q *db.Queries, workspaceID, projectID pgtype.UUID) (string, error) {
	var def *issueworkflow.Definition
	var err error
	if projectID.Valid {
		def, _, err = issueworkflow.ForProject(ctx, q, workspaceID, projectID)
	} else {
		def, err = issueworkflow.OutsideProject(ctx, q, workspaceID)
	}
	if err != nil {
		return "", err
	}
	if def != nil && def.Limits() && !def.Has(issuestatus.Backlog) {
		return issueworkflow.StartStatus, nil
	}
	return issuestatus.Backlog, nil
}

// closedTerminal reports whether a status is in a category only a closed
// GitHub issue can show.
func (m statusMapping) closedTerminal(key string) bool {
	switch m.category(key) {
	case issuestatus.CategoryDone, issuestatus.CategoryClosed:
		return true
	}
	return false
}

// labelKeys returns the statuses the issue's status labels map to, in label
// order and without repeats.
func (m statusMapping) labelKeys(labels []githubLabel) []string {
	var keys []string
	for _, label := range labels {
		key, ok := m.keyOfLabel[strings.ToLower(strings.TrimSpace(label.Name))]
		if !ok || m.closedTerminal(key) {
			continue
		}
		seen := false
		for _, k := range keys {
			seen = seen || k == key
		}
		if !seen {
			keys = append(keys, key)
		}
	}
	return keys
}

// fromGitHub returns the status GitHub's state calls for, given the issue's
// current status ("" for an issue not created yet). ambiguous is true when
// several status labels were present and none could be told newest.
func (m statusMapping) fromGitHub(state, stateReason string, labels []githubLabel, current string) (status string, ambiguous bool) {
	if state == "closed" {
		want, category := issuestatus.Done, issuestatus.CategoryDone
		if closedAsNotPlanned(stateReason) {
			want, category = issuestatus.Cancelled, issuestatus.CategoryClosed
		}
		if current != "" && m.category(current) == category {
			return current, false
		}
		return want, false
	}
	keys := m.labelKeys(labels)
	switch len(keys) {
	case 0:
		return m.entry, false
	case 1:
		return keys[0], false
	}
	var added []string
	for _, key := range keys {
		if key != current {
			added = append(added, key)
		}
	}
	if len(added) == 1 {
		return added[0], false
	}
	return added[len(added)-1], true
}

func closedAsNotPlanned(stateReason string) bool {
	return stateReason == "not_planned" || stateReason == "duplicate"
}

// mappable reports whether GitHub can show a status: the done and cancelled
// categories close the issue, the entry status is an open issue without a
// status label, and any other status needs a label in the project's map.
func (m statusMapping) mappable(key string) bool {
	if key == m.entry || m.closedTerminal(key) {
		return true
	}
	_, ok := m.labels[key]
	return ok
}

// shown lists what a status may be set to, for the rejection message.
func (m statusMapping) shown() []string {
	keys := []string{m.entry}
	for key := range m.labels {
		if key != m.entry {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys[1:])
	return keys
}

// UnmappedStatusError rejects a status write GitHub has no way to show: the
// status would be lost at the next poll. Agents read its message verbatim.
type UnmappedStatusError struct {
	Status   string
	Issue    string
	Mappable []string
}

func (e *UnmappedStatusError) Error() string {
	return fmt.Sprintf("status %q cannot be shown on GitHub issue %s: map it to a %q label in the project's GitHub settings, "+
		"or use one of %s, or a done or cancelled status", e.Status, e.Issue, StatusLabelPrefix+"<name>", strings.Join(e.Mappable, ", "))
}

// CheckGitHubStatus refuses to set a GitHub-backed issue to a status its
// project cannot show on GitHub. projectID is the project the issue is in
// after the write. Local issues, and writes that keep the status, pass.
func (s *Service) CheckGitHubStatus(ctx context.Context, issue db.Issue, projectID pgtype.UUID, status string) error {
	if status == "" || status == issue.Status {
		return nil
	}
	q, err := s.queries()
	if err != nil {
		return err
	}
	ext, err := q.GetIssueExternal(ctx, db.GetIssueExternalParams{IssueID: issue.ID, WorkspaceID: issue.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	m, err := loadStatusMapping(ctx, q, s.logger, issue.WorkspaceID, projectID)
	if err != nil {
		return err
	}
	if m.mappable(status) {
		return nil
	}
	return &UnmappedStatusError{
		Status:   status,
		Issue:    issuekey.GitHub(ext),
		Mappable: m.shown(),
	}
}

// StatusWriter moves an issue to the status GitHub calls for through the same
// entry as a person's status change (UpdateIssue), acting as userID, so the
// project workflow checks the status and hands the issue off.
type StatusWriter interface {
	WriteGitHubStatus(ctx context.Context, userID pgtype.UUID, issue db.Issue, status string) error
}

// StatusWriteError is a status write the issue write path refused, such as a
// status the project's workflow does not list. Retrying it would fail the
// same way.
type StatusWriteError struct {
	Code    int
	Message string
}

func (e *StatusWriteError) Error() string {
	return fmt.Sprintf("status write refused (%d): %s", e.Code, e.Message)
}

// SetStatusWriter installs the status write path. It is called once while
// the server is wired, before polling starts.
func (s *Service) SetStatusWriter(w StatusWriter) {
	s.statusWriter = w
}

type syncedWriteKey struct{}

// IsSyncedWrite reports whether ctx belongs to a write that brings Multica in
// line with GitHub, which must not be written back to GitHub.
func IsSyncedWrite(ctx context.Context) bool {
	synced, _ := ctx.Value(syncedWriteKey{}).(bool)
	return synced
}

// syncStatus moves an existing issue to the status GitHub's state calls for,
// when it differs from the current one. A write the issue write path refuses
// is logged and dropped, as retrying cannot succeed; any other failure is
// returned so the poller reads the issue again.
func (s *Service) syncStatus(ctx context.Context, repo *Repo, issue db.Issue, gh githubIssue) (bool, error) {
	q := db.New(s.db)
	m, err := loadStatusMapping(ctx, q, s.logger, issue.WorkspaceID, issue.ProjectID)
	if err != nil {
		return false, err
	}
	status, ambiguous := m.fromGitHub(gh.State, stringValue(gh.StateReason), gh.Labels, issue.Status)
	if ambiguous {
		s.logger.Warn("github issues: several status labels; took the last", "issue", repo.FullName(), "number", gh.Number, "status", status)
	}
	if status == issue.Status {
		return false, nil
	}
	if s.statusWriter == nil {
		s.logger.Warn("github issues: no status write path; status not synced", "issue", repo.FullName(), "number", gh.Number)
		return false, nil
	}
	actor, err := s.statusActor(ctx, issue.WorkspaceID)
	if err != nil {
		return false, err
	}
	err = s.statusWriter.WriteGitHubStatus(context.WithValue(ctx, syncedWriteKey{}, true), actor, issue, status)
	var refused *StatusWriteError
	if errors.As(err, &refused) {
		s.logger.Warn("github issues: status from GitHub refused", "issue", repo.FullName(), "number", gh.Number,
			"status", status, "code", refused.Code, "reason", refused.Message)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// statusActor is the member a status change from GitHub is recorded as: the
// sync account when it is a member, otherwise the workspace's earliest owner.
// A status write needs a member, as an agent acts only inside its own run.
func (s *Service) statusActor(ctx context.Context, workspaceID pgtype.UUID) (pgtype.UUID, error) {
	account, err := s.IssueCreator(ctx, workspaceID)
	if err != nil {
		return pgtype.UUID{}, err
	}
	if account.Type == "member" {
		return account.ID, nil
	}
	owner, err := db.New(s.db).GetGitHubSyncDefaultOwner(ctx, workspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, fmt.Errorf("%w (%s)", ErrSyncAccountUnavailable, EnvSyncAccount)
	}
	return owner, err
}
