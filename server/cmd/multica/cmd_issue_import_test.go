// Added by the mouse9527/multica fork: importing GitHub issues on request.

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func newIssueImportTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "import"}
	cmd.Flags().String("output", "json", "")
	return cmd
}

func TestIssueImportCommandRegistration(t *testing.T) {
	cmd, _, err := issueCmd.Find([]string{"import"})
	if err != nil || cmd != issueImportCmd {
		t.Fatalf("issue import not registered: %v", err)
	}
}

// The reference goes to the server as written: the server parses it, checks
// the repository against the workspace and answers the issue.
func TestRunIssueImportSendsReference(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.URL.Path != "/api/issues/import" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if ws := r.Header.Get("X-Workspace-ID"); ws != "ws-1" {
			t.Fatalf("X-Workspace-ID = %q", ws)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body) != 1 || body["ref"] != "https://github.com/acme/widgets/issues/7" {
			t.Fatalf("body = %v", body)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "1881a167-4bb6-4602-944b-f40ce4192fe6", "identifier": "acme/widgets#7",
			"title": "Crash on start", "status": "backlog", "provider": "github",
		})
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)

	out, err := captureStdout(t, func() error {
		return runIssueImport(newIssueImportTestCmd(), []string{" https://github.com/acme/widgets/issues/7 "})
	})
	if err != nil {
		t.Fatalf("runIssueImport: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil || got["identifier"] != "acme/widgets#7" || requests != 1 {
		t.Fatalf("stdout = %q (%v), requests = %d", out, err, requests)
	}

	cmd := newIssueImportTestCmd()
	_ = cmd.Flags().Set("output", "table")
	out, err = captureStdout(t, func() error {
		return runIssueImport(cmd, []string{"https://github.com/acme/widgets/issues/7"})
	})
	if err != nil || !strings.Contains(out, "acme/widgets#7") || !strings.Contains(out, "Crash on start") {
		t.Fatalf("table output = %q, %v", out, err)
	}
}

// A refusal reaches the user with the server's reason.
func TestRunIssueImportReportsRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"the repository is not registered on any project of this workspace","code":"github_repo_not_registered"}`))
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)

	err := runIssueImport(newIssueImportTestCmd(), []string{"acme/secrets#1"})
	if err == nil || !strings.Contains(err.Error(), "import issue") || !strings.Contains(err.Error(), "403") ||
		!strings.Contains(err.Error(), "not registered") {
		t.Fatalf("error = %v", err)
	}
}
