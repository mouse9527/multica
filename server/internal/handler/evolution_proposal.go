// Added by the mouse9527/multica fork: Evolution P3, proposals (M-137).

package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/multica-ai/multica/server/internal/evolution"
	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/projectaccess"
)

// maxProposalBody bounds a proposal request: two 16k-character diff sides
// plus the rest, in UTF-8.
const maxProposalBody = 256 << 10

// CreateEvolutionProposal files one improvement proposal from the Evolution
// agent's analysis run. Only the run's task may call it (the signal
// channel's gate). The server enforces the rules: field whitelist, evidence
// it may read from enough independent sources, quotes, redaction, names,
// the per-run cap, and the fingerprint. The same open proposal gets the new
// evidence as a comment instead of a second issue. Nothing is applied.
func (h *Handler) CreateEvolutionProposal(w http.ResponseWriter, r *http.Request) {
	workspaceID, agent, task, ok := h.evolutionRunTask(w, r, "file proposals")
	if !ok {
		return
	}
	ctx := r.Context()

	var proposal evolution.ProposalRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxProposalBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&proposal); err != nil {
		writeJSON(w, http.StatusBadRequest, evolution.RejectedError{
			Code: evolution.RejectInvalid, Message: "invalid request body: " + err.Error(),
		})
		return
	}
	st, err := evolution.LoadState(ctx, h.DB, workspaceID)
	if err != nil {
		h.evolutionFail(w, r, err)
		return
	}
	// The same hidden set as the signal channel: every private project.
	hidden, err := projectaccess.ForPrincipals(ctx, h.Queries, agent.WorkspaceID)
	if err != nil {
		h.evolutionFail(w, r, err)
		return
	}
	proposals := &evolution.Proposals{DB: h.evolutionDB(), Queries: h.Queries, Issues: h.IssueService, Bus: h.Bus}
	res, err := proposals.File(ctx, evolution.FileRequest{
		WorkspaceID:    workspaceID,
		AgentID:        uuidToString(agent.ID),
		AgentName:      agent.Name,
		IssueID:        uuidToString(task.IssueID),
		Proposal:       proposal,
		Config:         st.Config,
		HiddenProjects: hidden.Param(),
		CommentsActive: st.CommentsActive,
		ChatActive:     st.ChatActive,
		Now:            time.Now(),
	})
	var rejected *evolution.RejectedError
	if errors.As(err, &rejected) {
		status := http.StatusUnprocessableEntity
		switch rejected.Code {
		case evolution.RejectRunLimit, evolution.RejectRecentlyRejected:
			status = http.StatusConflict
		}
		slog.Info("evolution proposal rejected", append(logger.RequestAttrs(r), "workspace_id", workspaceID,
			"code", rejected.Code, "reason", rejected.Message)...)
		writeJSON(w, status, rejected)
		return
	}
	if err != nil {
		h.evolutionFail(w, r, err)
		return
	}
	status := http.StatusOK
	if res.Outcome == evolution.OutcomeCreated {
		status = http.StatusCreated
	}
	writeJSON(w, status, res)
}
