// Added by the mouse9527/multica fork: lets password management (M-41) tell
// a login session from the other credentials Auth accepts.

package middleware

import (
	"net/http"
	"strings"

	"github.com/multica-ai/multica/server/internal/auth"
)

// AuthenticatedBySession reports whether the credential Auth used for r is a
// login session JWT. It looks at the same token Auth does (the Authorization
// header first, else the auth cookie), so a personal access token, task token
// or cloud token is recognised wherever it was sent, cookie included. Call it
// only behind Auth, which has already validated the token.
func AuthenticatedBySession(r *http.Request) bool {
	token, _ := extractToken(r)
	if token == "" {
		return false
	}
	for _, prefix := range []string{"mul_", "mat_", auth.CloudPATPrefix} {
		if strings.HasPrefix(token, prefix) {
			return false
		}
	}
	// Auth treats every other token as a JWT and rejects it unless it
	// verifies; requiring the JWT shape keeps any future credential kind out
	// until it is classified here.
	return strings.Count(token, ".") == 2
}
