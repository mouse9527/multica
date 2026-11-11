package githubissues

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Choosing the repository of a new issue (docs/github-issues.md, "新建 issue
// 选仓库"). The candidates are the project's github_repo resources; the
// project's default is project_github_settings.default_resource_id.

// Provider values a create may name. "github" is the same as naming none.
const (
	ProviderLocal  = "local"
	ProviderGitHub = "github"
)

// Codes of a create whose repository could not be chosen.
const (
	// CodeRepoRequired: an agent named neither a repository nor
	// provider=local in a project that has GitHub repositories, though the
	// project would settle on one (its default or its only repository).
	CodeRepoRequired = "github_repo_required"
	// CodeRepoAmbiguous: the project has several GitHub repositories and no
	// default, and the create named none.
	CodeRepoAmbiguous = "github_repo_ambiguous"
	// CodeRepoNotRegistered: the named repository is on no project of the
	// workspace.
	CodeRepoNotRegistered = "github_repo_not_registered"
	// CodeProviderInvalid: an unknown provider, or a repository together
	// with provider=local.
	CodeProviderInvalid = "github_provider_invalid"
)

// ProjectRepo is a GitHub repository registered on a project.
type ProjectRepo struct {
	// ResourceID is the project_resource row that registers it.
	ResourceID pgtype.UUID
	// Repo is "owner/name" as registered.
	Repo string
}

// ProjectRepos returns the GitHub repositories registered on a project, in
// the project's resource order, and the project's default among them (nil
// when there is none or it no longer points at one of them). A repository
// registered twice is listed once, under its first resource.
func (s *Service) ProjectRepos(ctx context.Context, workspaceID, projectID pgtype.UUID) ([]ProjectRepo, *ProjectRepo, error) {
	q, err := s.queries()
	if err != nil {
		return nil, nil, err
	}
	resources, err := q.ListProjectResourcesInWorkspace(ctx, db.ListProjectResourcesInWorkspaceParams{
		ProjectID: projectID, WorkspaceID: workspaceID,
	})
	if err != nil {
		return nil, nil, err
	}
	var repos []ProjectRepo
	var names []repoName
	for _, res := range resources {
		if res.ResourceType != "github_repo" {
			continue
		}
		name, ok := repoNameFromResourceRef(res.ResourceRef)
		if !ok || containsName(names, name) {
			continue
		}
		names = append(names, name)
		repos = append(repos, ProjectRepo{ResourceID: res.ID, Repo: name.String()})
	}
	settings, err := q.GetProjectGithubSettings(ctx, db.GetProjectGithubSettingsParams{
		ProjectID: projectID, WorkspaceID: workspaceID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return repos, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	for i := range repos {
		if settings.DefaultResourceID.Valid && repos[i].ResourceID == settings.DefaultResourceID {
			return repos, &repos[i], nil
		}
	}
	return repos, nil, nil
}

// CreateStatuses lists the statuses a new GitHub issue of the project can be
// created in, its entry status first: GitHub opens every new issue, and an
// open issue shows the entry status without a label and any other status only
// through the label the project maps it to. Anything else is refused at create
// (UnmappedStatusError, or WriteNotAllowed for done and cancelled).
func (s *Service) CreateStatuses(ctx context.Context, workspaceID, projectID pgtype.UUID) ([]string, error) {
	q, err := s.queries()
	if err != nil {
		return nil, err
	}
	m, err := loadStatusMapping(ctx, q, s.logger, workspaceID, projectID)
	if err != nil {
		return nil, err
	}
	statuses := make([]string, 0, len(m.labels)+1)
	for _, key := range m.shown() {
		if !m.closedTerminal(key) {
			statuses = append(statuses, key)
		}
	}
	return statuses, nil
}

// ErrNotProjectRepo means the resource is not a GitHub repository registered
// on the project.
var ErrNotProjectRepo = errors.New("githubissues: the resource is not a GitHub repository of this project")

// SetProjectDefaultRepo makes the project's resource resourceID its default
// repository for new issues; an invalid resourceID clears the default. The
// status label mapping is kept.
func (s *Service) SetProjectDefaultRepo(ctx context.Context, workspaceID, projectID, resourceID pgtype.UUID) error {
	q, err := s.queries()
	if err != nil {
		return err
	}
	if resourceID.Valid {
		repos, _, err := s.ProjectRepos(ctx, workspaceID, projectID)
		if err != nil {
			return err
		}
		found := false
		for _, repo := range repos {
			found = found || repo.ResourceID == resourceID
		}
		if !found {
			return ErrNotProjectRepo
		}
	}
	_, err = q.SetProjectGithubDefaultResource(ctx, db.SetProjectGithubDefaultResourceParams{
		ProjectID: projectID, WorkspaceID: workspaceID, DefaultResourceID: resourceID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// The settings row belongs to another workspace; the project id was
		// checked against this one, so this does not happen in practice.
		return ErrNotProjectRepo
	}
	return err
}

// RepoChoice is what a create says about where the issue lives.
type RepoChoice struct {
	// Repo is the "owner/name" the request named, or "".
	Repo string
	// Provider is the provider the request named: "", "local" or "github".
	Provider string
	// ByAgent is true when an agent creates the issue. Agents never get a
	// repository chosen for them.
	ByAgent bool
}

// RepoChoiceError is a create whose repository could not be chosen. Nothing
// was written.
type RepoChoiceError struct {
	Code string
	// Candidates are the project's GitHub repositories ("owner/name").
	Candidates []string
	// Default is the project's default repository, or "".
	Default string
	msg     string
}

func (e *RepoChoiceError) Error() string { return "githubissues: " + e.msg }

// HTTPResponse is how a handler answers the create: 400 for a malformed
// choice, 422 with the candidates otherwise.
func (e *RepoChoiceError) HTTPResponse() (int, map[string]any) {
	status := http.StatusUnprocessableEntity
	if e.Code == CodeProviderInvalid {
		status = http.StatusBadRequest
	}
	candidates := e.Candidates
	if candidates == nil {
		candidates = []string{}
	}
	body := map[string]any{"code": e.Code, "error": e.msg, "candidates": candidates}
	if e.Default != "" {
		body["default_repo"] = e.Default
	}
	return status, body
}

// ResolveCreateRepo decides where a new issue in projectID lives: the
// repository to open it on, or "" for a local issue. In order:
//
//  1. provider=local: a local issue, as upstream.
//  2. A named repository: that one, if the workspace registers it.
//  3. No project, or a project without GitHub repositories: local.
//  4. An agent: refused (github_repo_required, or github_repo_ambiguous when
//     the project would not settle on one either).
//  5. The project's default repository, else its only one.
//  6. Otherwise refused (github_repo_ambiguous): the dialog asks.
func (s *Service) ResolveCreateRepo(ctx context.Context, workspaceID, projectID pgtype.UUID, choice RepoChoice) (string, error) {
	repo := strings.TrimSpace(choice.Repo)
	switch choice.Provider {
	case "", ProviderGitHub:
	case ProviderLocal:
		if repo != "" {
			return "", &RepoChoiceError{Code: CodeProviderInvalid, msg: "repo cannot be combined with provider \"local\""}
		}
		return "", nil
	default:
		return "", &RepoChoiceError{Code: CodeProviderInvalid, msg: "provider must be \"local\" or \"github\""}
	}

	var repos []ProjectRepo
	var def *ProjectRepo
	if projectID.Valid {
		var err error
		if repos, def, err = s.ProjectRepos(ctx, workspaceID, projectID); err != nil {
			return "", err
		}
	}
	refuse := func(code, msg string) *RepoChoiceError {
		e := &RepoChoiceError{Code: code, msg: msg, Candidates: make([]string, 0, len(repos))}
		for _, r := range repos {
			e.Candidates = append(e.Candidates, r.Repo)
		}
		if def != nil {
			e.Default = def.Repo
		}
		if len(e.Candidates) > 0 {
			e.msg += " (candidates: " + strings.Join(e.Candidates, ", ") + ")"
		}
		return e
	}

	if repo != "" {
		owner, name, _ := strings.Cut(repo, "/")
		named, ok := newRepoName(owner, name)
		if ok {
			if s.store == nil {
				return "", errors.New("githubissues: no database configured")
			}
			registered, err := s.registeredNames(ctx, workspaceID)
			if err != nil {
				return "", err
			}
			ok = containsName(registered, named)
		}
		if !ok {
			return "", refuse(CodeRepoNotRegistered, "repository "+repo+" is not registered on any project of this workspace")
		}
		return named.String(), nil
	}
	if choice.Provider == "" && len(repos) == 0 {
		return "", nil
	}
	if len(repos) == 0 {
		return "", refuse(CodeRepoRequired, "provider \"github\" needs a repository, and the issue's project has none")
	}
	settled := def
	if settled == nil && len(repos) == 1 {
		settled = &repos[0]
	}
	if choice.ByAgent {
		if settled == nil {
			return "", refuse(CodeRepoAmbiguous, "this project has several GitHub repositories; pass repo (owner/name), or provider \"local\" for a local issue")
		}
		return "", refuse(CodeRepoRequired, "this project has GitHub repositories; agents must pass repo (owner/name), or provider \"local\" for a local issue")
	}
	if settled == nil {
		return "", refuse(CodeRepoAmbiguous, "this project has several GitHub repositories and no default; choose one, or provider \"local\" for a local issue")
	}
	return settled.Repo, nil
}
