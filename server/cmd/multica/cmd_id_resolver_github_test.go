// Added by the mouse9527/multica fork: identifiers of GitHub-native issues.

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/cli"
)

func TestResolveIssueRefGitHubKey(t *testing.T) {
	issue := map[string]any{
		"id":         "1881a167-4bb6-4602-944b-f40ce4192fe6",
		"identifier": "octo/hello#7",
		"title":      "GitHub-backed",
	}
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		if gotPath != "/api/issues/octo%2Fhello%237" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(issue)
	}))
	defer srv.Close()

	client := cli.NewAPIClient(srv.URL, "ws-1", "test-token")
	got, err := resolveIssueRef(context.Background(), client, "octo/hello#7")
	if err != nil {
		t.Fatalf("resolveIssueRef: %v (server saw %q)", err, gotPath)
	}
	if got.ID != issue["id"] || got.Display != "octo/hello#7" {
		t.Fatalf("got %#v", got)
	}
}
