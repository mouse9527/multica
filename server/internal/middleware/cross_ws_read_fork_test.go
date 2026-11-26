// Added by the mouse9527/multica fork: cross-workspace read-only access (M-307).

package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/crossread"
)

const (
	crTarget = "11111111-1111-1111-1111-111111111111"
	crSource = "22222222-2222-2222-2222-222222222222"
	crAgent  = "33333333-3333-3333-3333-333333333333"
	crTask   = "44444444-4444-4444-4444-444444444444"
	crGrant  = "55555555-5555-5555-5555-555555555555"
)

type fakeCrossReadStore struct {
	enabled  bool
	grant    *crossread.Grant
	grantErr error
	auditErr error
	audits   []crossread.AuditEntry
	lookups  int
}

func (f *fakeCrossReadStore) Enabled(context.Context) bool { return f.enabled }

func (f *fakeCrossReadStore) ActiveGrant(_ context.Context, target, source, agent string) (crossread.Grant, bool, error) {
	f.lookups++
	if f.grantErr != nil {
		return crossread.Grant{}, false, f.grantErr
	}
	if f.grant == nil || f.grant.TargetWorkspaceID != target || f.grant.SourceWorkspaceID != source ||
		(f.grant.AgentID != "" && f.grant.AgentID != agent) {
		return crossread.Grant{}, false, nil
	}
	return *f.grant, true, nil
}

func (f *fakeCrossReadStore) RecordAudit(_ context.Context, e crossread.AuditEntry) error {
	if f.auditErr != nil {
		return f.auditErr
	}
	f.audits = append(f.audits, e)
	return nil
}

func liveGrant() *crossread.Grant {
	return &crossread.Grant{ID: crGrant, TargetWorkspaceID: crTarget, SourceWorkspaceID: crSource, AgentID: crAgent}
}

type crossReadResult struct {
	handled bool
	served  *http.Request
	rec     *httptest.ResponseRecorder
}

func runCrossRead(t *testing.T, store CrossWorkspaceReadStore, r *http.Request) crossReadResult {
	t.Helper()
	var res crossReadResult
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		res.served = r
		w.WriteHeader(http.StatusOK)
	})
	res.rec = httptest.NewRecorder()
	// Hand the store over the way the router does.
	WithCrossWorkspaceRead(store)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		res.handled = serveCrossWorkspaceRead(w, r, next, nil, crSource, crAgent, crTask)
	})).ServeHTTP(res.rec, r)
	return res
}

func requestFor(method, target, workspaceID string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	if workspaceID != "" {
		r.Header.Set("X-Workspace-ID", workspaceID)
	}
	return r
}

// Every way the feature can be absent leaves the request to the upstream
// check, which refuses a foreign workspace.
func TestCrossWorkspaceReadLeavesOtherRequestsToUpstream(t *testing.T) {
	cases := []struct {
		name  string
		store CrossWorkspaceReadStore
		req   *http.Request
	}{
		{"no store on the route group", nil, requestFor("GET", "/api/issues/x", crTarget)},
		{"flag off", &fakeCrossReadStore{grant: liveGrant()}, requestFor("GET", "/api/issues/x", crTarget)},
		{"no live grant", &fakeCrossReadStore{enabled: true}, requestFor("GET", "/api/issues/x", crTarget)},
		{"grant for another agent", &fakeCrossReadStore{enabled: true, grant: &crossread.Grant{
			ID: crGrant, TargetWorkspaceID: crTarget, SourceWorkspaceID: crSource, AgentID: crTask}},
			requestFor("GET", "/api/issues/x", crTarget)},
		{"no workspace named", &fakeCrossReadStore{enabled: true, grant: liveGrant()}, requestFor("GET", "/api/issues/x", "")},
		{"the bound workspace named", &fakeCrossReadStore{enabled: true, grant: liveGrant()}, requestFor("GET", "/api/issues/x", crSource)},
		{"selectors disagree", &fakeCrossReadStore{enabled: true, grant: liveGrant()},
			requestFor("GET", "/api/issues/x?workspace_id="+crSource, crTarget)},
		{"malformed selector", &fakeCrossReadStore{enabled: true, grant: liveGrant()}, requestFor("GET", "/api/issues/x", "not-a-uuid")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := runCrossRead(t, tc.store, tc.req)
			if res.handled || res.served != nil {
				t.Fatalf("handled=%v served=%v, want the request left to upstream", res.handled, res.served != nil)
			}
			if f, ok := tc.store.(*fakeCrossReadStore); ok && len(f.audits) != 0 {
				t.Fatalf("audited %d requests no grant matched", len(f.audits))
			}
		})
	}
}

func TestCrossWorkspaceReadServesAllowlistedReads(t *testing.T) {
	for _, tc := range []struct {
		method, path, pattern string
	}{
		{"GET", "/api/issues/abc", "GET /api/issues/*"},
		{"GET", "/api/issues/abc/comments", "GET /api/issues/*/comments"},
		{"POST", "/api/issues/query", "POST /api/issues/query"},
		{"GET", "/api/tasks/abc/messages", "GET /api/tasks/*/messages"},
		{"GET", "/api/workspaces/" + crTarget, "GET /api/workspaces/*"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			store := &fakeCrossReadStore{enabled: true, grant: liveGrant()}
			r := requestFor(tc.method, tc.path, crTarget)
			r.Header.Set("X-Agent-ID", crAgent)
			r.Header.Set("X-Task-ID", crTask)
			res := runCrossRead(t, store, r)
			if !res.handled || res.served == nil {
				t.Fatalf("not served: %d %s", res.rec.Code, res.rec.Body.String())
			}
			h := res.served.Header
			if h.Get("X-User-ID") != crGrant || h.Get("X-Workspace-ID") != crTarget ||
				h.Get("X-Actor-Source") != ActorSourceCrossWorkspaceReader || h.Get("X-Agent-ID") != "" || h.Get("X-Task-ID") != "" {
				t.Fatalf("served as user=%q ws=%q source=%q agent=%q task=%q", h.Get("X-User-ID"), h.Get("X-Workspace-ID"),
					h.Get("X-Actor-Source"), h.Get("X-Agent-ID"), h.Get("X-Task-ID"))
			}
			if g, ok := crossWorkspaceReadGrant(res.served.Context()); !ok || g.ID != crGrant {
				t.Fatal("the grant is not on the served request's context")
			}
			want := crossread.AuditEntry{GrantID: crGrant, TargetWorkspaceID: crTarget, SourceWorkspaceID: crSource,
				AgentID: crAgent, TaskID: crTask, Method: tc.method, Route: tc.pattern, Allowed: true}
			if len(store.audits) != 1 || store.audits[0] != want {
				t.Fatalf("audit = %+v, want [%+v]", store.audits, want)
			}
		})
	}
}

func TestCrossWorkspaceReadRefusesEverythingElse(t *testing.T) {
	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/issues"},
		{"PUT", "/api/issues/abc"},
		{"DELETE", "/api/issues/abc"},
		{"POST", "/api/issues/abc/comments"},
		{"PUT", "/api/comments/abc"},
		{"GET", "/api/agents/abc/env"},
		{"GET", "/api/agents/abc/mcp-servers"},
		{"GET", "/api/chat/sessions"},
		{"GET", "/api/inbox"},
		{"POST", "/api/issues/abc/environments/def/logs"},
		{"GET", "/api/workspaces/" + crTarget + "/cross-read-grants"},
		{"GET", "/api/runtimes"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			store := &fakeCrossReadStore{enabled: true, grant: liveGrant()}
			res := runCrossRead(t, store, requestFor(tc.method, tc.path, crTarget))
			if !res.handled || res.served != nil || res.rec.Code != http.StatusForbidden {
				t.Fatalf("handled=%v served=%v status=%d, want 403", res.handled, res.served != nil, res.rec.Code)
			}
			if len(store.audits) != 1 || store.audits[0].Allowed || store.audits[0].Route != tc.method+" "+tc.path {
				t.Fatalf("audit = %+v, want one refused entry for the request", store.audits)
			}
		})
	}
}

// The allowlist is matched on the path chi routes on, so an encoded "/"
// cannot make the two see different routes.
func TestCrossWorkspaceReadMatchesTheEscapedPath(t *testing.T) {
	store := &fakeCrossReadStore{enabled: true, grant: liveGrant()}
	res := runCrossRead(t, store, requestFor("GET", "/api/issues/a%2Fb/comments", crTarget))
	if res.served == nil || len(store.audits) != 1 || store.audits[0].Route != "GET /api/issues/*/comments" {
		t.Fatalf("served=%v audits=%+v", res.served != nil, store.audits)
	}
}

func TestCrossWorkspaceReadFailsClosed(t *testing.T) {
	t.Run("grant lookup fails", func(t *testing.T) {
		store := &fakeCrossReadStore{enabled: true, grantErr: errors.New("db down")}
		res := runCrossRead(t, store, requestFor("GET", "/api/issues/abc", crTarget))
		if !res.handled || res.served != nil || res.rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("handled=%v served=%v status=%d, want 503", res.handled, res.served != nil, res.rec.Code)
		}
	})
	t.Run("audit write fails", func(t *testing.T) {
		store := &fakeCrossReadStore{enabled: true, grant: liveGrant(), auditErr: errors.New("db down")}
		res := runCrossRead(t, store, requestFor("GET", "/api/issues/abc", crTarget))
		if !res.handled || res.served != nil || res.rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("handled=%v served=%v status=%d, want 503", res.handled, res.served != nil, res.rec.Code)
		}
	})
}

// The allowlist holds reads only: GET, plus the few POST routes that read
// with a filter set too large for a query string.
func TestCrossWorkspaceReadAllowlistIsReadOnly(t *testing.T) {
	postReads := map[string]bool{
		"POST /api/issues/query":        true,
		"POST /api/issues/table/groups": true,
		"POST /api/issues/table/rows":   true,
		"POST /api/issues/table/facets": true,
	}
	for _, route := range crossWorkspaceReadRoutes {
		p := route.pattern()
		if route.method != http.MethodGet && !postReads[p] {
			t.Errorf("%s is not a read", p)
		}
		for _, banned := range []string{"/api/chat", "/api/inbox", "/env", "mcp", "environments", "cross-read", "/deliveries", "/apps"} {
			if strings.Contains(p, banned) {
				t.Errorf("%s reaches %s, which the reader must not read", p, banned)
			}
		}
	}
}

func TestCrossWorkspaceReaderMember(t *testing.T) {
	grant := *liveGrant()
	next := func(served **http.Request) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { *served = r })
	}
	reader := func() *http.Request {
		r := httptest.NewRequest("GET", "/api/issues", nil)
		r.Header.Set("X-Actor-Source", ActorSourceCrossWorkspaceReader)
		return r.WithContext(context.WithValue(r.Context(), ctxKeyCrossReadGrant, grant))
	}

	t.Run("other requests pass through", func(t *testing.T) {
		var served *http.Request
		if serveCrossWorkspaceReaderMember(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil), next(&served), crTarget, nil) {
			t.Fatal("handled a request not served under a grant")
		}
	})
	t.Run("runs as the synthetic member", func(t *testing.T) {
		var served *http.Request
		rec := httptest.NewRecorder()
		if !serveCrossWorkspaceReaderMember(rec, reader(), next(&served), strings.ToUpper(crTarget), nil) || served == nil {
			t.Fatalf("not served: %d", rec.Code)
		}
		m, ok := MemberFromContext(served.Context())
		if !ok || m.Role != RoleCrossWorkspaceReader || m.UserID != m.ID {
			t.Fatalf("member = %+v", m)
		}
		if WorkspaceIDFromContext(served.Context()) != crTarget {
			t.Fatalf("workspace = %q", WorkspaceIDFromContext(served.Context()))
		}
		if got, ok := CrossWorkspaceReaderMember(served.Context(), crGrant, crTarget); !ok || got != m {
			t.Fatal("handlers do not get the same member")
		}
		if _, ok := CrossWorkspaceReaderMember(served.Context(), crAgent, crTarget); ok {
			t.Fatal("another user id got the reader's member")
		}
		if _, ok := CrossWorkspaceReaderMember(served.Context(), crGrant, crSource); ok {
			t.Fatal("the reader is a member of another workspace")
		}
	})
	t.Run("another workspace is refused", func(t *testing.T) {
		var served *http.Request
		rec := httptest.NewRecorder()
		if !serveCrossWorkspaceReaderMember(rec, reader(), next(&served), crSource, nil) || served != nil || rec.Code != http.StatusForbidden {
			t.Fatalf("status %d served=%v, want 403", rec.Code, served != nil)
		}
	})
	t.Run("role-gated groups are refused", func(t *testing.T) {
		var served *http.Request
		rec := httptest.NewRecorder()
		if !serveCrossWorkspaceReaderMember(rec, reader(), next(&served), crTarget, []string{"owner", "admin", "member"}) || served != nil || rec.Code != http.StatusForbidden {
			t.Fatalf("status %d served=%v, want 403", rec.Code, served != nil)
		}
	})
	t.Run("a forged actor source without a grant is refused", func(t *testing.T) {
		var served *http.Request
		rec := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/api/issues", nil)
		r.Header.Set("X-Actor-Source", ActorSourceCrossWorkspaceReader)
		if !serveCrossWorkspaceReaderMember(rec, r, next(&served), crTarget, nil) || served != nil || rec.Code != http.StatusForbidden {
			t.Fatalf("status %d served=%v, want 403", rec.Code, served != nil)
		}
	})
}
