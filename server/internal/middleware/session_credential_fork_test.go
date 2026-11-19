// Added by the mouse9527/multica fork (M-41).

package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/auth"
)

func TestAuthenticatedBySession(t *testing.T) {
	const jwtShaped = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.c2lnbmF0dXJl"
	for _, tc := range []struct {
		name   string
		bearer string
		cookie string
		want   bool
	}{
		{name: "no credential"},
		{name: "JWT bearer", bearer: jwtShaped, want: true},
		{name: "JWT cookie", cookie: jwtShaped, want: true},
		{name: "PAT bearer", bearer: "mul_0123456789abcdef"},
		{name: "PAT cookie", cookie: "mul_0123456789abcdef"},
		{name: "task token cookie", cookie: "mat_0123456789abcdef"},
		{name: "cloud token cookie", cookie: auth.CloudPATPrefix + "0123456789abcdef"},
		{name: "PAT bearer wins over JWT cookie", bearer: "mul_0123456789abcdef", cookie: jwtShaped},
		{name: "JWT bearer wins over PAT cookie", bearer: jwtShaped, cookie: "mul_0123456789abcdef", want: true},
		{name: "unknown kind", cookie: "xyz_not.a.jwt.shape"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/me/password", nil)
			if tc.bearer != "" {
				r.Header.Set("Authorization", "Bearer "+tc.bearer)
			}
			if tc.cookie != "" {
				r.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: tc.cookie})
			}
			if got := AuthenticatedBySession(r); got != tc.want {
				t.Fatalf("AuthenticatedBySession = %v, want %v", got, tc.want)
			}
		})
	}
}
