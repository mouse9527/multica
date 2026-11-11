// Added by the mouse9527/multica fork: `issue create --repo/--provider`.
// Upstream has no such file.

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// issueCreateGitHubServer answers POST /api/issues with status and payload,
// and records each create body.
func issueCreateGitHubServer(t *testing.T, status int, payload map[string]any) *[]map[string]any {
	t.Helper()
	// Away from any daemon task marker in the checkout's parents.
	t.Chdir(t.TempDir())
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/issues" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		bodies = append(bodies, body)
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	t.Setenv("MULTICA_TOKEN", "test-token")
	return &bodies
}

func TestRunIssueCreateSendsRepoAndProvider(t *testing.T) {
	created := map[string]any{"id": "issue-1", "identifier": "acme/widgets#7", "title": "T", "status": "backlog", "priority": "none"}
	for _, tc := range []struct {
		name  string
		flags map[string]string
		want  map[string]any
	}{
		{"repo", map[string]string{"repo": "acme/widgets"}, map[string]any{"repo": "acme/widgets"}},
		{"provider local", map[string]string{"provider": "local"}, map[string]any{"provider": "local"}},
		{"neither", nil, map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bodies := issueCreateGitHubServer(t, http.StatusCreated, created)
			cmd := newIssueCreateTestCmd()
			registerIssueCreateGitHubFlags(cmd)
			_ = cmd.Flags().Set("title", "T")
			for k, v := range tc.flags {
				_ = cmd.Flags().Set(k, v)
			}
			if err := runIssueCreate(cmd, nil); err != nil {
				t.Fatalf("runIssueCreate: %v", err)
			}
			body := (*bodies)[0]
			for _, key := range []string{"repo", "provider"} {
				if body[key] != tc.want[key] {
					t.Fatalf("%s = %#v, want %#v (body %v)", key, body[key], tc.want[key], body)
				}
			}
		})
	}
}

func TestRunIssueCreateRejectsBadGitHubFlagsBeforePost(t *testing.T) {
	for _, tc := range []struct {
		flags map[string]string
		want  string
	}{
		{map[string]string{"provider": "gitlab"}, "invalid --provider"},
		{map[string]string{"repo": "acme/widgets", "provider": "local"}, "cannot be combined"},
		{map[string]string{"repo": "widgets"}, "expected owner/name"},
		{map[string]string{"repo": "acme/widgets/extra"}, "expected owner/name"},
	} {
		bodies := issueCreateGitHubServer(t, http.StatusCreated, map[string]any{})
		cmd := newIssueCreateTestCmd()
		registerIssueCreateGitHubFlags(cmd)
		_ = cmd.Flags().Set("title", "T")
		for k, v := range tc.flags {
			_ = cmd.Flags().Set(k, v)
		}
		err := runIssueCreate(cmd, nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("flags %v: error = %v, want %q", tc.flags, err, tc.want)
		}
		if len(*bodies) != 0 {
			t.Fatalf("flags %v: sent %d create(s)", tc.flags, len(*bodies))
		}
	}
}

// The server's 422 names the candidates; the CLI shows each as the flag to
// pass, so an agent can retry without guessing.
func TestRunIssueCreateShowsRepoCandidates(t *testing.T) {
	issueCreateGitHubServer(t, http.StatusUnprocessableEntity, map[string]any{
		"code":         "github_repo_required",
		"error":        "this project has GitHub repositories; agents must pass repo (owner/name), or provider \"local\" for a local issue (candidates: acme/widgets, acme/gadgets)",
		"candidates":   []string{"acme/widgets", "acme/gadgets"},
		"default_repo": "acme/gadgets",
	})
	cmd := newIssueCreateTestCmd()
	registerIssueCreateGitHubFlags(cmd)
	_ = cmd.Flags().Set("title", "T")
	err := runIssueCreate(cmd, nil)
	if err == nil {
		t.Fatal("expected the refusal")
	}
	for _, want := range []string{
		"[github_repo_required]",
		"\n  --repo acme/widgets\n",
		"\n  --repo acme/gadgets (project default)\n",
		"\n  --provider local",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
}
