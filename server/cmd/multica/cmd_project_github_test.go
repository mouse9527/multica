// Added by the mouse9527/multica fork: `project github-settings`. Upstream
// has no such file.

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

const githubSettingsProjectID = "11111111-2222-3333-4444-555555555555"

func newGitHubSettingsSetTestCmd(flags map[string][]string) *cobra.Command {
	cmd := &cobra.Command{Use: "set"}
	registerProjectGitHubSettingsSetFlags(cmd)
	for name, values := range flags {
		for _, v := range values {
			_ = cmd.Flags().Set(name, v)
		}
	}
	return cmd
}

// githubSettingsServer answers GET and PUT /api/projects/{id}/github-settings
// for one project: GET with settings, PUT with putStatus and putPayload.
// It records each PUT body.
func githubSettingsServer(t *testing.T, settings map[string]any, putStatus int, putPayload any) *[]map[string]any {
	t.Helper()
	t.Chdir(t.TempDir())
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/projects/"+githubSettingsProjectID+"/github-settings" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(settings)
		case http.MethodPut:
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode body: %v", err)
			}
			bodies = append(bodies, body)
			w.WriteHeader(putStatus)
			_ = json.NewEncoder(w).Encode(putPayload)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	t.Setenv("MULTICA_TOKEN", "test-token")
	return &bodies
}

var githubSettingsFixture = map[string]any{
	"repos": []any{
		map[string]any{"resource_id": "res-widgets", "repo": "acme/widgets"},
		map[string]any{"resource_id": "res-gadgets", "repo": "acme/gadgets"},
	},
	"default_resource_id": nil,
	"create_statuses":     []any{"backlog", "in_review"},
	"status_label_map":    map[string]any{"in_review": "status:review"},
	"mappable_statuses":   []any{"todo", "in_progress", "in_review", "blocked"},
}

func TestProjectGitHubSettingsSetBody(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags map[string][]string
		want  map[string]any
		err   string
	}{
		{name: "labels", flags: map[string][]string{"status-label": {"todo=status:todo", " in_review = status:review "}},
			want: map[string]any{"status_label_map": map[string]string{"todo": "status:todo", "in_review": "status:review"}}},
		{name: "clear labels", flags: map[string][]string{"clear-status-labels": {"true"}},
			want: map[string]any{"status_label_map": map[string]string{}}},
		{name: "default repo, any case", flags: map[string][]string{"default-repo": {"Acme/Gadgets"}},
			want: map[string]any{"default_resource_id": "res-gadgets"}},
		{name: "clear default and labels", flags: map[string][]string{"clear-default-repo": {"true"}, "status-label": {"blocked=status:blocked"}},
			want: map[string]any{"default_resource_id": nil, "status_label_map": map[string]string{"blocked": "status:blocked"}}},
		{name: "nothing", flags: nil, err: "nothing to change"},
		{name: "both default flags", flags: map[string][]string{"default-repo": {"acme/widgets"}, "clear-default-repo": {"true"}}, err: "cannot be combined"},
		{name: "both label flags", flags: map[string][]string{"status-label": {"todo=status:todo"}, "clear-status-labels": {"true"}}, err: "cannot be combined"},
		{name: "no equals", flags: map[string][]string{"status-label": {"todo"}}, err: "expected key=status:<name>"},
		{name: "empty label", flags: map[string][]string{"status-label": {"todo="}}, err: "expected key=status:<name>"},
		{name: "repeated key", flags: map[string][]string{"status-label": {"todo=status:a", "todo=status:b"}}, err: "twice"},
		{name: "bad repo", flags: map[string][]string{"default-repo": {"widgets"}}, err: "expected owner/name"},
		{name: "repo not on project", flags: map[string][]string{"default-repo": {"acme/other"}}, err: "its repositories: acme/widgets, acme/gadgets"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			read := 0
			body, err := projectGitHubSettingsBody(newGitHubSettingsSetTestCmd(tc.flags), func() (projectGitHubSettings, error) {
				read++
				var s projectGitHubSettings
				raw, _ := json.Marshal(githubSettingsFixture)
				return s, json.Unmarshal(raw, &s)
			})
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if !reflect.DeepEqual(body, tc.want) {
				t.Fatalf("body = %#v, want %#v", body, tc.want)
			}
			if _, named := tc.flags["default-repo"]; !named && read != 0 {
				t.Fatal("settings read without --default-repo")
			}
		})
	}
}

func TestRunProjectGitHubSettingsSet(t *testing.T) {
	saved := map[string]any{}
	for k, v := range githubSettingsFixture {
		saved[k] = v
	}
	saved["default_resource_id"] = "res-widgets"
	bodies := githubSettingsServer(t, githubSettingsFixture, http.StatusOK, saved)
	cmd := newGitHubSettingsSetTestCmd(map[string][]string{"default-repo": {"acme/widgets"}, "output": {"json"}})
	out, err := captureStdout(t, func() error { return runProjectGitHubSettingsSet(cmd, []string{githubSettingsProjectID}) })
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if len(*bodies) != 1 || !reflect.DeepEqual((*bodies)[0], map[string]any{"default_resource_id": "res-widgets"}) {
		t.Fatalf("PUT bodies = %v", *bodies)
	}
	var printed map[string]any
	if err := json.Unmarshal([]byte(out), &printed); err != nil || printed["default_resource_id"] != "res-widgets" {
		t.Fatalf("printed = %q (%v), want the saved settings", out, err)
	}
}

// A refused map comes back as the server said it, with the statuses that can
// be mapped.
func TestRunProjectGitHubSettingsSetRefused(t *testing.T) {
	githubSettingsServer(t, githubSettingsFixture, http.StatusBadRequest, map[string]any{
		"code":              "github_status_label_invalid",
		"error":             `label "review" of status "in_review" must be "status:" followed by a name`,
		"mappable_statuses": []string{"todo", "in_progress", "in_review", "blocked"},
	})
	cmd := newGitHubSettingsSetTestCmd(map[string][]string{"status-label": {"in_review=review"}})
	err := runProjectGitHubSettingsSet(cmd, []string{githubSettingsProjectID})
	if err == nil {
		t.Fatal("set succeeded, want the refusal")
	}
	for _, want := range []string{
		`label "review" of status "in_review" must be "status:" followed by a name [github_status_label_invalid]`,
		"statuses that can be mapped: todo, in_progress, in_review, blocked",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %q, want it to contain %q", err, want)
		}
	}
}

func TestRunProjectGitHubSettingsGetFeatureOff(t *testing.T) {
	t.Chdir(t.TempDir())
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	t.Setenv("MULTICA_SERVER_URL", srv.URL)
	t.Setenv("MULTICA_WORKSPACE_ID", "ws-1")
	t.Setenv("MULTICA_TOKEN", "test-token")
	cmd := &cobra.Command{Use: "get"}
	cmd.Flags().String("output", "table", "")
	err := runProjectGitHubSettingsGet(cmd, []string{githubSettingsProjectID})
	if err == nil || !strings.Contains(err.Error(), "GitHub-native issues are off") {
		t.Fatalf("err = %v, want the feature-off explanation", err)
	}
}

func TestWriteProjectGitHubSettingsTable(t *testing.T) {
	var s projectGitHubSettings
	raw, _ := json.Marshal(githubSettingsFixture)
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	writeProjectGitHubSettings(&b, s)
	for _, want := range []string{
		"Repositories:       acme/widgets, acme/gadgets",
		"Default repository: (none)",
		"Mappable statuses:  todo, in_progress, in_review, blocked",
		"in_review", "status:review",
	} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("table = %q, want %q", b.String(), want)
		}
	}
}
