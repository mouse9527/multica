// Added by the mouse9527/multica fork: Evolution structural analysis, the
// metrics source (M-296).

package handler

import (
	"net/http"
	"strconv"

	"github.com/multica-ai/multica/server/internal/evolution"
	"github.com/multica-ai/multica/server/internal/projectaccess"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// serveEvolutionMetrics answers GET /api/evolution/signals?source=metrics for
// the open run's task that evolutionRunTask admitted. days (1-30, default 14)
// is how far back from the run's window_until the summary reaches. It is
// served only while sources.metrics is on, hides every private project as
// the other sources do, and leaves out excluded projects and agents.
func (h *Handler) serveEvolutionMetrics(w http.ResponseWriter, r *http.Request, workspaceID string, agent db.Agent, task db.AgentTaskQueue) {
	ctx := r.Context()
	req := evolution.MetricsRequest{
		WorkspaceID: workspaceID,
		IssueID:     uuidToString(task.IssueID),
		Days:        evolution.DefaultMetricsWindowDays,
		SelfAgentID: uuidToString(agent.ID),
	}
	if days := r.URL.Query().Get("days"); days != "" {
		n, err := strconv.Atoi(days)
		if err != nil || n < evolution.MinMetricsWindowDays || n > evolution.MaxMetricsWindowDays {
			writeError(w, http.StatusBadRequest, "days must be between "+strconv.Itoa(evolution.MinMetricsWindowDays)+
				" and "+strconv.Itoa(evolution.MaxMetricsWindowDays))
			return
		}
		req.Days = n
	}
	st, err := evolution.LoadState(ctx, h.DB, workspaceID)
	if err != nil {
		h.evolutionFail(w, r, err)
		return
	}
	req.Config = st.Config
	hidden, err := projectaccess.ForPrincipals(ctx, h.Queries, agent.WorkspaceID)
	if err != nil {
		h.evolutionFail(w, r, err)
		return
	}
	req.HiddenProjects = hidden.Param()

	summary, err := evolution.ServeMetrics(ctx, h.DB, req)
	if err != nil {
		h.evolutionFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}
