// Added by the mouse9527/multica fork (M-237): tells "this token does not
// exist" apart from "we could not look it up right now" in Auth.

package middleware

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
)

// errAuthBackendUnavailable is the 503 body Auth returns when a token lookup
// failed for a reason other than the token not existing.
const errAuthBackendUnavailable = "auth backend unavailable"

// rejectTokenLookupError writes the response for a failed token lookup in the
// mat_ and mul_ branches of Auth.
//
// Only pgx.ErrNoRows means the token is unknown (or expired, for task tokens,
// whose query filters on expires_at) and earns a 401. Any other error — the
// database unreachable, a timeout, a cancelled context — says nothing about
// the token, so it gets a 503 the caller can retry. Answering 401 there made
// the CLI tell a running agent its task token was rejected for good, and the
// agent gave up on a token that was still valid (M-236). The cloud PAT branch
// already draws the same line ("cloud pat verifier unavailable" → 503).
//
// msg is the existing log message for the branch; the 401 body is unchanged.
func rejectTokenLookupError(w http.ResponseWriter, r *http.Request, msg string, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		slog.Warn(msg, "path", r.URL.Path, "error", err)
		http.Error(w, `{"error":"invalid token"}`, http.StatusUnauthorized)
		return
	}
	slog.Warn(msg+": lookup unavailable", "path", r.URL.Path, "error", err)
	writeError(w, http.StatusServiceUnavailable, errAuthBackendUnavailable)
}
