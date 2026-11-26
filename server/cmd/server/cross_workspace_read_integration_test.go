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
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/featureflags"
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
	defer srv.Close()
	on := crossReadCall{t: t, server: srv.URL}
	// The shared test server runs without feature flags: upstream behavior.
	off := crossReadCall{t: t, server: testServer.URL}

	// The target workspace, owned by the test user, with a public issue and
	// an issue in a private project.
	slug := fmt.Sprintf("cross-read-%d", time.Now().UnixNano())
	target := crossReadID(t, on.want(http.StatusCreated, "POST", "/api/workspaces", testToken, "",
		map[string]any{"name": "Cross Read Target", "slug": slug}))
	t.Cleanup(func() {
		on.do("DELETE", "/api/workspaces/"+target, testToken, "", nil)
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
