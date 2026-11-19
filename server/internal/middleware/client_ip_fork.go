// Added by the mouse9527/multica fork: exposes the rate limiter's client
// address rule to password login (internal/passwordauth), which keeps its
// own in-process limit because it must work without Redis.

package middleware

import (
	"net"
	"net/http"
)

// ClientIP returns the client address the same way RateLimit does: the
// rightmost untrusted X-Forwarded-For entry when the request came through a
// trusted proxy, RemoteAddr otherwise.
func ClientIP(r *http.Request, trustedProxies []*net.IPNet) string {
	return extractIP(r, trustedProxies)
}
