// Added by the mouse9527/multica fork: the GitHub settings of a project for
// GitHub-native issues (server/internal/githubissues). Upstream has no such
// file.

package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/githubissues"
	"github.com/multica-ai/multica/server/internal/logger"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ProjectGitHubRepoResponse is a GitHub repository registered on a project.
type ProjectGitHubRepoResponse struct {
	ResourceID string `json:"resource_id"`
	Repo       string `json:"repo"`
}

// ProjectGitHubSettingsResponse is what the create dialog needs to offer a
// repository: the project's GitHub repositories and its default among them.
type ProjectGitHubSettingsResponse struct {
	Repos             []ProjectGitHubRepoResponse `json:"repos"`
	DefaultResourceID *string                     `json:"default_resource_id"`
}

// SetProjectGitHubSettingsRequest sets the project's default repository; null
// clears it.
type SetProjectGitHubSettingsRequest struct {
	DefaultResourceID *string `json:"default_resource_id"`
}

// GetProjectGitHubSettings serves GET /api/projects/{id}/github-settings. The
// route is mounted only while the feature is on, so clients see 404 and offer
// no repository while it is off.
func (h *Handler) GetProjectGitHubSettings(w http.ResponseWriter, r *http.Request) {
	project, ok := h.loadProjectForResource(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	h.writeProjectGitHubSettings(w, r, project)
}

// SetProjectGitHubSettings serves PUT /api/projects/{id}/github-settings. Any
// member who may edit the project's resources may choose the default among
// them.
func (h *Handler) SetProjectGitHubSettings(w http.ResponseWriter, r *http.Request) {
	project, ok := h.loadProjectForResource(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	if _, ok := requireUserID(w, r); !ok {
		return
	}
	var req SetProjectGitHubSettingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var resourceID pgtype.UUID
	if req.DefaultResourceID != nil {
		if resourceID, ok = parseUUIDOrBadRequest(w, *req.DefaultResourceID, "default_resource_id"); !ok {
			return
		}
	}
	err := h.GitHubIssues.SetProjectDefaultRepo(r.Context(), project.WorkspaceID, project.ID, resourceID)
	if errors.Is(err, githubissues.ErrNotProjectRepo) {
		writeError(w, http.StatusBadRequest, "default_resource_id is not a GitHub repository of this project")
		return
	}
	if err != nil {
		slog.Warn("set project github default failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to save the project's GitHub settings")
		return
	}
	h.writeProjectGitHubSettings(w, r, project)
}

func (h *Handler) writeProjectGitHubSettings(w http.ResponseWriter, r *http.Request, project db.Project) {
	repos, def, err := h.GitHubIssues.ProjectRepos(r.Context(), project.WorkspaceID, project.ID)
	if err != nil {
		slog.Warn("list project github repositories failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to read the project's GitHub settings")
		return
	}
	resp := ProjectGitHubSettingsResponse{Repos: make([]ProjectGitHubRepoResponse, 0, len(repos))}
	for _, repo := range repos {
		resp.Repos = append(resp.Repos, ProjectGitHubRepoResponse{ResourceID: uuidToString(repo.ResourceID), Repo: repo.Repo})
	}
	if def != nil {
		id := uuidToString(def.ResourceID)
		resp.DefaultResourceID = &id
	}
	writeJSON(w, http.StatusOK, resp)
}
