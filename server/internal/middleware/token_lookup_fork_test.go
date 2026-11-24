// Added by the mouse9527/multica fork (M-237).

package middleware

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// failingDBTX answers every query with err, standing in for a database that
// either has no matching row (pgx.ErrNoRows) or cannot be reached at all.
type failingDBTX struct{ err error }

func (f failingDBTX) Exec(context.Context, string, ...interface{}) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, f.err
}

func (f failingDBTX) Query(context.Context, string, ...interface{}) (pgx.Rows, error) {
	return nil, f.err
}

func (f failingDBTX) QueryRow(context.Context, string, ...interface{}) pgx.Row {
	return failingRow{f.err}
}

type failingRow struct{ err error }

func (r failingRow) Scan(...any) error { return r.err }

// TestAuth_TokenLookupFailureStatus pins which lookup failures are the
// token's fault. An unknown task token or PAT is a 401; a lookup that failed
// for any other reason is a 503, because the CLI turns a 401 on a task token
// into "this token was rejected, do not retry" and the agent then gives up
// on a token that is still valid (M-236).
func TestAuth_TokenLookupFailureStatus(t *testing.T) {
	dbDown := fmt.Errorf("failed to connect to `host=db.example.invalid`: hostname resolving error: %w",
		errors.New("lookup db.example.invalid: no such host"))

	cases := []struct {
		name     string
		token    string
		err      error
		wantCode int
		wantBody string
	}{
		{"task token unknown", "mat_unknown", pgx.ErrNoRows, http.StatusUnauthorized, `{"error":"invalid token"}`},
		{"task token, database unreachable", "mat_valid", dbDown, http.StatusServiceUnavailable, `{"error":"auth backend unavailable"}`},
		{"task token, request cancelled", "mat_valid", context.Canceled, http.StatusServiceUnavailable, `{"error":"auth backend unavailable"}`},
		{"task token, lookup timed out", "mat_valid", context.DeadlineExceeded, http.StatusServiceUnavailable, `{"error":"auth backend unavailable"}`},
		{"PAT unknown", "mul_unknown", pgx.ErrNoRows, http.StatusUnauthorized, `{"error":"invalid token"}`},
		{"PAT, database unreachable", "mul_valid", dbDown, http.StatusServiceUnavailable, `{"error":"auth backend unavailable"}`},
		// sqlc returns the scan error as is; a wrapped ErrNoRows is still
		// "no such token".
		{"task token unknown, wrapped", "mat_unknown", fmt.Errorf("scan: %w", pgx.ErrNoRows), http.StatusUnauthorized, `{"error":"invalid token"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			queries := db.New(failingDBTX{err: tc.err})
			handler := Auth(queries, nil, nil, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Fatal("next handler must not run when the token lookup failed")
			}))

			req := httptest.NewRequest("GET", "/api/me", nil)
			req.Header.Set("Authorization", "Bearer "+tc.token)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body %q)", w.Code, tc.wantCode, w.Body.String())
			}
			if got := strings.TrimSpace(w.Body.String()); got != tc.wantBody {
				t.Fatalf("body = %q, want %q", got, tc.wantBody)
			}
		})
	}
}
