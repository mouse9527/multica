// Added by the mouse9527/multica fork: cross-workspace read-only access (M-307).

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/pkg/featureflag"
)

type crossReadCall struct {
	t      *testing.T
	server string
}

// do sends one request and returns the status and body. token is a JWT or a
// mat_ token; workspaceID, when set, goes in X-Workspace-ID.
func (c crossReadCall) do(method, path, token, workspaceID string, body any) (int, string) {
	c.t.Helper()
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.server+path, reader)
	if err != nil {
		c.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	if workspaceID != "" {
		req.Header.Set("X-Workspace-ID", workspaceID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

func (c crossReadCall) want(status int, method, path, token, workspaceID string, body any) string {
	c.t.Helper()
	got, out := c.do(method, path, token, workspaceID, body)
	if got != status {
		c.t.Fatalf("%s %s: status %d, want %d: %s", method, path, got, status, out)
	}
	return out
}

func crossReadID(t *testing.T, body string) string {
	t.Helper()
	var v struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil || v.ID == "" {
		t.Fatalf("no id in %s: %v", body, err)
	}
	return v.ID
}

// TestCrossWorkspaceRead drives the whole feature through the real router:
// an agent of the test workspace (the source) reads a second workspace (the
// target) with its own task token.
func TestCrossWorkspaceRead(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}
	ctx := context.Background()

	provider := featureflag.NewStaticProvider()
	provider.Set(featureflags.CrossWorkspaceRead, featureflag.Rule{Default: true})
	router, _ := NewRouterWithOptions(testPool, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil,
		RouterOptions{FeatureFlags: featureflag.NewService(provider)})
	srv := httptest.NewServer(router)
	// Registered first so it runs last: the cleanups below still talk to it.
	t.Cleanup(srv.Close)
	on := crossReadCall{t: t, server: srv.URL}
	// The shared test server runs without feature flags: upstream behavior.
	off := crossReadCall{t: t, server: testServer.URL}

	// The target workspace, owned by the test user, with a public issue and
	// an issue in a private project.
	slug := fmt.Sprintf("cross-read-%d", time.Now().UnixNano())
	target := crossReadID(t, on.want(http.StatusCreated, "POST", "/api/workspaces", testToken, "",
		map[string]any{"name": "Cross Read Target", "slug": slug}))
	t.Cleanup(func() {
		// Best effort through the API, which also tears down the
		// workspace's other rows; a failure here must not fail the test.
		if req, err := http.NewRequest("DELETE", srv.URL+"/api/workspaces/"+target, nil); err == nil {
			req.Header.Set("Authorization", "Bearer "+testToken)
			if resp, err := http.DefaultClient.Do(req); err == nil {
				resp.Body.Close()
			}
		}
		testPool.Exec(ctx, `DELETE FROM workspace WHERE id = $1`, target)
		testPool.Exec(ctx, `DELETE FROM cross_workspace_read_audit WHERE target_workspace_id = $1`, target)
		testPool.Exec(ctx, `DELETE FROM cross_workspace_read_grant WHERE target_workspace_id = $1`, target)
	})
	publicIssue := crossReadID(t, on.want(http.StatusCreated, "POST", "/api/issues", testToken, target,
		map[string]any{"title": "Readable across workspaces"}))
	project := crossReadID(t, on.want(http.StatusCreated, "POST", "/api/projects", testToken, target,
		map[string]any{"title": "Private project"}))
	privateIssue := crossReadID(t, on.want(http.StatusCreated, "POST", "/api/issues", testToken, target,
		map[string]any{"title": "Inside a private project", "project_id": project}))
	// Made private directly: creating a private project is a plan feature
	// the test instance need not have.
	if _, err := testPool.Exec(ctx, `UPDATE project SET visibility = 'private' WHERE id = $1`, project); err != nil {
		t.Fatalf("make project private: %v", err)
	}

	// An agent run of the source workspace. The token's user owns the
	// target, which must not matter: the token is bound to the source.
	sourceIssue, agentID, taskID := setupSweeperTestFixture(t, "running")
	t.Cleanup(func() { cleanupSweeperFixture(t, sourceIssue, agentID) })
	token := mintAgentTaskToken(t, agentID, taskID, testUserID)

	// No grant: refused as upstream refuses it, and the source still works.
	on.want(http.StatusForbidden, "GET", "/api/issues/"+publicIssue, token, target, nil)
	on.want(http.StatusOK, "GET", "/api/issues/"+sourceIssue, token, "", nil)

	// Only a person who owns or administers the target can grant.
	on.want(http.StatusForbidden, "POST", "/api/workspaces/"+target+"/cross-read-grants", token, target,
		map[string]any{"source_workspace_id": testWorkspaceID})
	on.want(http.StatusBadRequest, "POST", "/api/workspaces/"+target+"/cross-read-grants", testToken, "",
		map[string]any{"source_workspace_id": target})
	grant := crossReadID(t, on.want(http.StatusCreated, "POST", "/api/workspaces/"+target+"/cross-read-grants", testToken, "",
		map[string]any{"source_workspace_id": testWorkspaceID, "agent_id": agentID, "note": "troubleshooting"}))

	// Flag off: the grant is ignored and the routes do not exist.
	off.want(http.StatusForbidden, "GET", "/api/issues/"+publicIssue, token, target, nil)
	off.want(http.StatusNotFound, "GET", "/api/workspaces/"+target+"/cross-read-grants", testToken, "", nil)

	// Reads in the target.
	on.want(http.StatusOK, "GET", "/api/issues/"+publicIssue, token, target, nil)
	on.want(http.StatusOK, "GET", "/api/issues/"+publicIssue+"/comments", token, target, nil)
	on.want(http.StatusOK, "GET", "/api/issues/"+publicIssue+"/timeline", token, target, nil)
	on.want(http.StatusOK, "GET", "/api/issue-statuses", token, target, nil)
	on.want(http.StatusOK, "GET", "/api/workspaces/"+target, token, target, nil)
	list := on.want(http.StatusOK, "GET", "/api/issues?workspace_id="+target, token, "", nil)
	if !strings.Contains(list, publicIssue) {
		t.Fatalf("issue list misses the public issue: %s", list)
	}
	if strings.Contains(list, privateIssue) {
		t.Fatalf("issue list shows the private project's issue: %s", list)
	}

	// Private projects stay hidden.
	on.want(http.StatusNotFound, "GET", "/api/issues/"+privateIssue, token, target, nil)
	on.want(http.StatusNotFound, "GET", "/api/projects/"+project, token, target, nil)

	// Writes and anything off the allowlist are refused.
	on.want(http.StatusForbidden, "POST", "/api/issues/"+publicIssue+"/comments", token, target,
		map[string]any{"content": "written from another workspace"})
	on.want(http.StatusForbidden, "PUT", "/api/issues/"+publicIssue, token, target,
		map[string]any{"title": "renamed"})
	on.want(http.StatusForbidden, "POST", "/api/issues", token, target, map[string]any{"title": "new"})
	on.want(http.StatusForbidden, "GET", "/api/agents/"+agentID+"/env", token, target, nil)
	on.want(http.StatusForbidden, "GET", "/api/chat/sessions", token, target, nil)
	on.want(http.StatusForbidden, "GET", "/api/workspaces/"+target+"/cross-read-grants", token, target, nil)
	var comments int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM comment WHERE issue_id = $1`, publicIssue).Scan(&comments); err != nil {
		t.Fatalf("count comments: %v", err)
	}
	if comments != 0 {
		t.Fatalf("a refused write left %d comments", comments)
	}

	// The source workspace is unaffected by the grant.
	on.want(http.StatusOK, "GET", "/api/issues/"+sourceIssue, token, "", nil)

	// Every request the grant matched is audited with the run that made it.
	var allowed, refused, wrongRun int
	if err := testPool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE allowed),
		       count(*) FILTER (WHERE NOT allowed),
		       count(*) FILTER (WHERE agent_id <> $2 OR task_id <> $3 OR source_workspace_id <> $4)
		FROM cross_workspace_read_audit WHERE grant_id = $1`,
		grant, agentID, taskID, testWorkspaceID).Scan(&allowed, &refused, &wrongRun); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if allowed != 8 || refused != 6 || wrongRun != 0 {
		t.Fatalf("audit: %d allowed, %d refused, %d with the wrong run; want 8, 6, 0", allowed, refused, wrongRun)
	}
	var route string
	if err := testPool.QueryRow(ctx, `
		SELECT route FROM cross_workspace_read_audit
		WHERE grant_id = $1 AND allowed AND method = 'GET'
		ORDER BY created_at LIMIT 1`, grant).Scan(&route); err != nil {
		t.Fatalf("read audit route: %v", err)
	}
	if route != "GET /api/issues/*" {
		t.Fatalf("audit route = %q, want the allowlist pattern", route)
	}
	audit := on.want(http.StatusOK, "GET", "/api/workspaces/"+target+"/cross-read-grants/audit?limit=5", testToken, "", nil)
	var rows []map[string]any
	if err := json.Unmarshal([]byte(audit), &rows); err != nil || len(rows) != 5 {
		t.Fatalf("audit listing: %d rows (%v): %s", len(rows), err, audit)
	}

	// Attachments: files on visible issues and files outside any issue
	// (docs) are readable; chat files, including a chat scoped to the
	// private project and a chat run's upload, and files on the private
	// project's issues are not.
	chatSession := func(projectID any) string {
		t.Helper()
		var id string
		if err := testPool.QueryRow(ctx, `
			INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, project_id)
			VALUES ($1, $2, $3, 'cross read chat', $4) RETURNING id::text`,
			target, agentID, testUserID, projectID).Scan(&id); err != nil {
			t.Fatalf("insert chat session: %v", err)
		}
		return id
	}
	attachment := func(issueID, chatSessionID, chatTaskID any) string {
		t.Helper()
		var id string
		if err := testPool.QueryRow(ctx, `
			INSERT INTO attachment (workspace_id, issue_id, chat_session_id, task_id, uploader_type, uploader_id, filename, url, content_type, size_bytes)
			VALUES ($1, $2, $3, $4, 'member', $5, 'f.txt', 'https://example.com/f.txt', 'text/plain', 1)
			RETURNING id::text`, target, issueID, chatSessionID, chatTaskID, testUserID).Scan(&id); err != nil {
			t.Fatalf("insert attachment: %v", err)
		}
		return id
	}
	on.want(http.StatusOK, "GET", "/api/attachments/"+attachment(publicIssue, nil, nil), token, target, nil)
	on.want(http.StatusOK, "GET", "/api/attachments/"+attachment(nil, nil, nil), token, target, nil)
	for name, id := range map[string]string{
		"chat":                 attachment(nil, chatSession(nil), nil),
		"private project chat": attachment(nil, chatSession(project), nil),
		"chat run upload":      attachment(nil, nil, taskID),
		"private issue":        attachment(privateIssue, nil, nil),
	} {
		for _, path := range []string{"/api/attachments/" + id, "/api/attachments/" + id + "/content"} {
			if got, out := on.do("GET", path, token, target, nil); got != http.StatusNotFound {
				t.Fatalf("%s attachment: GET %s = %d, want 404: %s", name, path, got, out)
			}
		}
	}

	// Every allowlisted route that names an issue hides the private one, and
	// no allowlisted route writes: the target's issues, comments and
	// activity are unchanged after calling all of them.
	counts := func() [3]int {
		t.Helper()
		var c [3]int
		if err := testPool.QueryRow(ctx, `
			SELECT (SELECT count(*) FROM issue WHERE workspace_id = $1),
			       (SELECT count(*) FROM comment WHERE workspace_id = $1),
			       (SELECT count(*) FROM activity_log WHERE workspace_id = $1)`, target).Scan(&c[0], &c[1], &c[2]); err != nil {
			t.Fatalf("count target rows: %v", err)
		}
		return c
	}
	before := counts()
	for _, pattern := range middleware.CrossWorkspaceReadPatterns() {
		method, path, _ := strings.Cut(pattern, " ")
		issueScoped := strings.HasPrefix(path, "/api/issues/*")
		switch {
		case issueScoped:
			path = strings.Replace(path, "*", privateIssue, 1)
		case strings.HasPrefix(path, "/api/workspaces/*"):
			path = strings.Replace(path, "*", target, 1)
		}
		path = strings.ReplaceAll(path, "*", "00000000-0000-0000-0000-000000000001")
		var body any
		if method == http.MethodPost {
			body = map[string]any{}
		}
		got, out := on.do(method, path+"?workspace_id="+target, token, "", body)
		if got >= 500 || (got == http.StatusForbidden && strings.Contains(out, "read-only routes only")) {
			t.Errorf("%s %s: status %d: %s", method, path, got, out)
		}
		if issueScoped && got != http.StatusNotFound {
			t.Errorf("%s %s on the private issue: status %d, want 404: %s", method, path, got, out)
		}
	}
	if after := counts(); after != before {
		t.Fatalf("allowlisted routes wrote to the target: before %v, after %v", before, after)
	}

	// Revoking takes effect on the next request.
	on.want(http.StatusOK, "DELETE", "/api/workspaces/"+target+"/cross-read-grants/"+grant, testToken, "", nil)
	on.want(http.StatusForbidden, "GET", "/api/issues/"+publicIssue, token, target, nil)

	// So does expiry, by the database clock.
	expiring := crossReadID(t, on.want(http.StatusCreated, "POST", "/api/workspaces/"+target+"/cross-read-grants", testToken, "",
		map[string]any{"source_workspace_id": testWorkspaceID, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}))
	on.want(http.StatusOK, "GET", "/api/issues/"+publicIssue, token, target, nil)
	if _, err := testPool.Exec(ctx, `
		UPDATE cross_workspace_read_grant
		SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 second'
		WHERE id = $1`, expiring); err != nil {
		t.Fatalf("expire grant: %v", err)
	}
	on.want(http.StatusForbidden, "GET", "/api/issues/"+publicIssue, token, target, nil)
}

var crossReadRouteParam = regexp.MustCompile(`\{[^}]+\}`)

// TestCrossWorkspaceReadAllowlistAgainstTheRouter walks every route the
// router registers. The allowlist gate runs in Auth, before routing, so what
// it admits is decided by matching each registered route here: every route it
// admits must be a read (GET, or one of the POST reads), every allowlist entry
// must still name a registered route, and everything else is refused.
func TestCrossWorkspaceReadAllowlistAgainstTheRouter(t *testing.T) {
	router, _ := NewRouterWithOptions(testPool, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil, RouterOptions{})
	postReads := map[string]bool{
		"POST /api/issues/query":        true,
		"POST /api/issues/table/groups": true,
		"POST /api/issues/table/rows":   true,
		"POST /api/issues/table/facets": true,
	}
	hit := map[string]bool{}
	admitted, refused := 0, 0
	err := chi.Walk(router, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		path := crossReadRouteParam.ReplaceAllString(pattern, "x")
		if strings.Contains(path, "*") {
			refused++ // a catch-all; the gate sees the concrete path and its allowlist has none
			return nil
		}
		entry, ok := middleware.CrossWorkspaceReadRoute(method, path)
		if !ok {
			refused++
			return nil
		}
		admitted++
		hit[entry] = true
		if method != http.MethodGet && !postReads[entry] {
			t.Errorf("%s %s is admitted by %q but is not a read", method, pattern, entry)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk router: %v", err)
	}
	for _, entry := range middleware.CrossWorkspaceReadPatterns() {
		if !hit[entry] {
			t.Errorf("allowlist entry %q names no registered route", entry)
		}
	}
	if admitted == 0 || refused == 0 {
		t.Fatalf("walked %d admitted and %d refused routes", admitted, refused)
	}
	t.Logf("cross-workspace reader: %d routes admitted, %d refused", admitted, refused)
}
