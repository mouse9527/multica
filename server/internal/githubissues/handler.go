package githubissues

import (
	"encoding/json"
	"net/http"
)

// HandleTokenStatus serves the read-only token status. It is mounted only
// while the feature is on, so a server with the feature off answers 404 like
// upstream.
func (s *Service) HandleTokenStatus(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(s.TokenStatus())
}
