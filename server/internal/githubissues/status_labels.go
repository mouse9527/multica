package githubissues

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Configuring a project's status labels (docs/github-issues.md, "状态映射 ·
// 怎么配置"). loadStatusMapping reads what is stored here; this file checks
// what may be stored.

// Codes of a status label map the project's GitHub settings refuse.
const (
	// CodeStatusLabelKeyInvalid: a key is not a status of the project's
	// workflow, is the entry status (an open issue without a label), or is
	// in the done or cancelled category (only closing the issue shows those).
	CodeStatusLabelKeyInvalid = "github_status_label_key_invalid"
	// CodeStatusLabelInvalid: a label does not start with "status:", has
	// nothing after the prefix, or is longer than GitHub allows.
	CodeStatusLabelInvalid = "github_status_label_invalid"
	// CodeStatusLabelDuplicate: one label (without regard to case) is mapped
	// to two keys.
	CodeStatusLabelDuplicate = "github_status_label_duplicate"
)

// maxLabelLen is the longest label name GitHub accepts.
const maxLabelLen = 50

// StatusLabelMapError is a status label map the project's settings refuse.
// Nothing was written.
type StatusLabelMapError struct {
	Code string
	// Mappable are the statuses of the project's workflow a label may stand
	// for (MappableStatuses).
	Mappable []string
	msg      string
}

func (e *StatusLabelMapError) Error() string { return "githubissues: " + e.msg }

// HTTPResponse is how a handler answers the refused map: 400 with the code
// and the statuses that may be mapped.
func (e *StatusLabelMapError) HTTPResponse() (int, map[string]any) {
	mappable := e.Mappable
	if mappable == nil {
		mappable = []string{}
	}
	return http.StatusBadRequest, map[string]any{"code": e.Code, "error": e.msg, "mappable_statuses": mappable}
}

// MappableStatuses lists, in workflow order, the statuses of the project's
// workflow a status label may stand for: every status but the entry status
// and those in the done and cancelled categories.
func (s *Service) MappableStatuses(ctx context.Context, workspaceID, projectID pgtype.UUID) ([]string, error) {
	q, err := s.queries()
	if err != nil {
		return nil, err
	}
	return mappableStatuses(ctx, q, workspaceID, projectID)
}

func mappableStatuses(ctx context.Context, q *db.Queries, workspaceID, projectID pgtype.UUID) ([]string, error) {
	def, _, err := issueworkflow.ForProject(ctx, q, workspaceID, projectID)
	if err != nil || def == nil {
		return []string{}, err
	}
	entry, err := entryStatus(ctx, q, workspaceID, projectID)
	if err != nil {
		return nil, err
	}
	keys := []string{}
	for _, key := range def.StatusKeys() {
		if key == entry {
			continue
		}
		category, err := issuestatus.CategoryWithError(ctx, q, workspaceID, key)
		if err != nil {
			return nil, err
		}
		if category != issuestatus.CategoryDone && category != issuestatus.CategoryClosed {
			keys = append(keys, key)
		}
	}
	return keys, nil
}

// StatusLabelMap returns the project's status label map as stored: an empty
// map when the project has none, or when what is stored is not an object of
// strings (loadStatusMapping ignores that as well).
func (s *Service) StatusLabelMap(ctx context.Context, workspaceID, projectID pgtype.UUID) (map[string]string, error) {
	q, err := s.queries()
	if err != nil {
		return nil, err
	}
	labels := map[string]string{}
	settings, err := q.GetProjectGithubSettings(ctx, db.GetProjectGithubSettingsParams{ProjectID: projectID, WorkspaceID: workspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return labels, nil
	}
	if err != nil {
		return nil, err
	}
	if len(settings.StatusLabelMap) > 0 && json.Unmarshal(settings.StatusLabelMap, &labels) != nil {
		return map[string]string{}, nil
	}
	return labels, nil
}

// checkStatusLabelMap validates a status label map for the project and
// returns it with the labels trimmed.
func checkStatusLabelMap(ctx context.Context, q *db.Queries, workspaceID, projectID pgtype.UUID, labels map[string]string) (map[string]string, error) {
	mappable, err := mappableStatuses(ctx, q, workspaceID, projectID)
	if err != nil {
		return nil, err
	}
	refuse := func(code, format string, args ...any) error {
		msg := fmt.Sprintf(format, args...)
		if code == CodeStatusLabelKeyInvalid {
			if len(mappable) == 0 {
				msg += "; the project's workflow has no status a label can stand for"
			} else {
				msg += "; statuses that can be mapped: " + strings.Join(mappable, ", ")
			}
		}
		return &StatusLabelMapError{Code: code, Mappable: mappable, msg: msg}
	}
	// Sorted, so which of several problems is reported does not depend on
	// map order.
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	checked := make(map[string]string, len(labels))
	keyOfLabel := map[string]string{}
	for _, key := range keys {
		ok := false
		for _, m := range mappable {
			ok = ok || m == key
		}
		if !ok {
			return nil, refuse(CodeStatusLabelKeyInvalid, "status %q cannot have a status label: it must be a status of the project's workflow "+
				"other than its entry status and the done and cancelled statuses", key)
		}
		label := strings.TrimSpace(labels[key])
		lower := strings.ToLower(label)
		if !strings.HasPrefix(lower, StatusLabelPrefix) || strings.TrimSpace(label[len(StatusLabelPrefix):]) == "" {
			return nil, refuse(CodeStatusLabelInvalid, "label %q of status %q must be %q followed by a name", label, key, StatusLabelPrefix)
		}
		if len([]rune(label)) > maxLabelLen {
			return nil, refuse(CodeStatusLabelInvalid, "label %q of status %q is longer than GitHub's %d characters", label, key, maxLabelLen)
		}
		if other, taken := keyOfLabel[lower]; taken {
			return nil, refuse(CodeStatusLabelDuplicate, "label %q is mapped to both %q and %q", label, other, key)
		}
		keyOfLabel[lower] = key
		checked[key] = label
	}
	return checked, nil
}

// ProjectSettingsUpdate is a change to a project's GitHub settings. A part
// whose Set flag is false is kept.
type ProjectSettingsUpdate struct {
	SetDefault bool
	// DefaultResourceID is the new default repository; invalid clears it.
	DefaultResourceID pgtype.UUID
	SetStatusLabels   bool
	// StatusLabelMap is the new status label map; empty clears it.
	StatusLabelMap map[string]string
}

// UpdateProjectSettings applies u to the project's GitHub settings in one
// transaction: either every part is saved or, when one is refused
// (ErrNotProjectRepo, *StatusLabelMapError), none is.
func (s *Service) UpdateProjectSettings(ctx context.Context, workspaceID, projectID pgtype.UUID, u ProjectSettingsUpdate) error {
	q, err := s.queries()
	if err != nil {
		return err
	}
	if u.SetDefault && u.DefaultResourceID.Valid {
		repos, _, err := s.ProjectRepos(ctx, workspaceID, projectID)
		if err != nil {
			return err
		}
		found := false
		for _, repo := range repos {
			found = found || repo.ResourceID == u.DefaultResourceID
		}
		if !found {
			return ErrNotProjectRepo
		}
	}
	var labelMap []byte
	if u.SetStatusLabels {
		checked, err := checkStatusLabelMap(ctx, q, workspaceID, projectID, u.StatusLabelMap)
		if err != nil {
			return err
		}
		if labelMap, err = json.Marshal(checked); err != nil {
			return err
		}
	}
	if !u.SetDefault && !u.SetStatusLabels {
		return nil
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	qtx := q.WithTx(tx)
	if u.SetDefault {
		_, err = qtx.SetProjectGithubDefaultResource(ctx, db.SetProjectGithubDefaultResourceParams{
			ProjectID: projectID, WorkspaceID: workspaceID, DefaultResourceID: u.DefaultResourceID,
		})
	}
	if err == nil && u.SetStatusLabels {
		_, err = qtx.SetProjectGithubStatusLabelMap(ctx, db.SetProjectGithubStatusLabelMapParams{
			ProjectID: projectID, WorkspaceID: workspaceID, StatusLabelMap: labelMap,
		})
	}
	if errors.Is(err, pgx.ErrNoRows) {
		// The settings row belongs to another workspace; the project id was
		// checked against this one, so this does not happen in practice.
		return ErrNotProjectRepo
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
