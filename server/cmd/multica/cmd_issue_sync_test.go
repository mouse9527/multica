// Added by the mouse9527/multica fork: syncing GitHub issues on request.

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func newIssueSyncTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "sync"}
	cmd.Flags().String("project", "", "")
	cmd.Flags().Bool("open-only", false, "")
	cmd.Flags().String("output", "json", "")
	return cmd
}

func TestIssueSyncCommandRegistration(t *testing.T) {
	cmd, _, err := issueCmd.Find([]string{"sync"})
	if err != nil || cmd != issueSyncCmd {
		t.Fatalf("issue sync not registered: %v", err)
	}
}

func TestRunIssueSyncValidatesArguments(t *testing.T) {
	setCLITestServerEnv(t, "http://127.0.0.1:0")
	both := newIssueSyncTestCmd()
	_ = both.Flags().Set("project", "11111111-1111-4111-8111-111111111111")
	onlyOpen := newIssueSyncTestCmd()
	_ = onlyOpen.Flags().Set("open-only", "true")
	for name, c := range map[string]struct {
		cmd  *cobra.Command
		args []string
	}{
		"nothing":           {newIssueSyncTestCmd(), nil},
		"issue and project": {both, []string{"acme/widgets#1"}},
		"open-only issue":   {onlyOpen, []string{"acme/widgets#1"}},
	} {
		if err := runIssueSync(c.cmd, c.args); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// One issue: the reference goes to the server as written, escaped into the
// path, and the answer is printed.
func TestRunIssueSyncOne(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.EscapedPath())
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issue_id": "1881a167-4bb6-4602-944b-f40ce4192fe6", "identifier": "acme/widgets#7",
			"issue_updated": true, "new_comments": 2, "comments_updated": 0, "pulls_updated": 0, "requests_used": 3,
		})
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)

	out, err := captureStdout(t, func() error { return runIssueSync(newIssueSyncTestCmd(), []string{"acme/widgets#7"}) })
	if err != nil {
		t.Fatalf("runIssueSync: %v", err)
	}
	if len(paths) != 1 || paths[0] != "POST /api/issues/acme%2Fwidgets%237/github-sync" {
		t.Fatalf("requests = %v", paths)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil || got["new_comments"] != float64(2) {
		t.Fatalf("stdout = %q (%v)", out, err)
	}
}

// A project sync goes through the project's issues one at a time and stops
// at the first refusal of the quota guard, reporting how far it got; an
// issue that fails for its own reasons does not stop it.
func TestRunIssueSyncProjectStopsOnLowQuota(t *testing.T) {
	const project = "11111111-1111-4111-8111-111111111111"
	var synced []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			if r.URL.Path != "/api/projects/"+project+"/github-issues" || r.URL.Query().Get("open_only") != "true" {
				t.Errorf("list request = %s?%s", r.URL.Path, r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"issues": []map[string]any{
				{"id": "a", "identifier": "acme/widgets#1", "state": "open"},
				{"id": "b", "identifier": "acme/widgets#2", "state": "open"},
				{"id": "c", "identifier": "acme/widgets#3", "state": "open"},
				{"id": "d", "identifier": "acme/widgets#4", "state": "open"},
				{"id": "e", "identifier": "acme/widgets#5", "state": "open"},
			}})
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/issues/"), "/github-sync")
		synced = append(synced, id)
		switch id {
		case "a":
			_ = json.NewEncoder(w).Encode(map[string]any{"identifier": "acme/widgets#1", "issue_updated": true, "requests_used": 3,
				"rate_limit": map[string]any{"remaining": 1001}})
		case "b":
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "github_sync_failed", "error": "failed", "requests_used": 1})
		case "c":
			_ = json.NewEncoder(w).Encode(map[string]any{"identifier": "acme/widgets#3", "requests_used": 2})
		default:
			w.Header().Set("Retry-After", "1200")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "github_quota_low", "error": "quota low",
				"remaining": 998, "retry_after_seconds": 1200, "requests_used": 0})
		}
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)

	cmd := newIssueSyncTestCmd()
	_ = cmd.Flags().Set("project", project)
	_ = cmd.Flags().Set("open-only", "true")
	out, err := captureStdout(t, func() error { return runIssueSync(cmd, nil) })
	if err == nil || !strings.Contains(err.Error(), "stopped after 3 of 5") {
		t.Fatalf("error = %v", err)
	}
	if strings.Join(synced, ",") != "a,b,c,d" {
		t.Fatalf("synced %v, want a,b,c then the refusal at d", synced)
	}
	var got projectSyncSummary
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout = %q: %v", out, err)
	}
	if got.Total != 5 || got.Synced != 2 || got.Changed != 1 || got.Failed != 1 || got.NotAttempted != 2 ||
		got.RequestsUsed != 6 || got.Stopped["code"] != "github_quota_low" || got.Stopped["identifier"] != "acme/widgets#4" {
		t.Fatalf("summary = %+v", got)
	}
}
