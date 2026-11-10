// Added by the mouse9527/multica fork: identifiers of GitHub-native issues.

package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/multica-ai/multica/server/internal/githubissues/issuekey"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const issueKeyTestPrefix = "IKT"

// setIssueKeysEnabledForTest flips the process-wide GitHub-native issues
// switch that identifier rendering follows, restoring it when the test ends.
func setIssueKeysEnabledForTest(t *testing.T, on bool) {
	t.Helper()
	previous := issuekey.Enabled()
	issuekey.SetEnabled(on)
	t.Cleanup(func() { issuekey.SetEnabled(previous) })
}

// linkGitHubIssue gives an existing issue a GitHub identity, the way the
// poller will once it lands.
func linkGitHubIssue(t *testing.T, issueID, repo string, number int) {
	t.Helper()
	dbfx.InsertNoID(t, "issue_external", testutil.Cols{
		"issue_id":            issueID,
		"workspace_id":        testWorkspaceID,
		"provider":            "github",
		"external_repo_id":    424242,
		"external_repo":       repo,
		"external_number":     number,
		"external_node_id":    "I_node_" + issueID,
		"state":               "open",
		"external_updated_at": testutil.Raw("now()"),
	}, "issue_id = $1", issueID)
}

func issueKeyTestNumber(t *testing.T, issueID string) int {
	t.Helper()
	var n int
	dbfx.QueryRow(t, `SELECT number FROM issue WHERE id = $1`, issueID).Scan(&n)
	return n
}

func getIssueForKeyTest(t *testing.T, h *Handler, ref string) IssueResponse {
	t.Helper()
	var resp IssueResponse
	testutil.Call(t, h.GetIssue, withURLParam(newRequest(http.MethodGet, "/api/issues/"+url.PathEscape(ref), nil), "id", ref)).
		Want(http.StatusOK).JSON(&resp)
	return resp
}

func TestIssueKey_LocalIssueUnchanged(t *testing.T) {
	setWorkspaceIssuePrefixForTest(t, issueKeyTestPrefix)
	issueID := dbfx.Issue(t, "local issue")
	want := issueKeyTestPrefix + "-" + strconv.Itoa(issueKeyTestNumber(t, issueID))

	got := getIssueForKeyTest(t, testHandler, issueID)
	if got.Identifier != want || got.Provider != "local" {
		t.Fatalf("local issue: identifier=%q provider=%q, want %q local", got.Identifier, got.Provider, want)
	}
	// The local key still resolves.
	if byKey := getIssueForKeyTest(t, testHandler, want); byKey.ID != issueID {
		t.Fatalf("GET %s resolved to %s, want %s", want, byKey.ID, issueID)
	}
}

func TestIssueKey_GitHubIssueDetailAndResolve(t *testing.T) {
	setIssueKeysEnabledForTest(t, true)
	setWorkspaceIssuePrefixForTest(t, issueKeyTestPrefix)
	issueID := dbfx.Issue(t, "github issue")
	linkGitHubIssue(t, issueID, "Octo-Org/hello.world", 7)
	localKey := issueKeyTestPrefix + "-" + strconv.Itoa(issueKeyTestNumber(t, issueID))

	got := getIssueForKeyTest(t, testHandler, issueID)
	if got.Identifier != "Octo-Org/hello.world#7" || got.Provider != "github" {
		t.Fatalf("github issue: identifier=%q provider=%q, want Octo-Org/hello.world#7 github", got.Identifier, got.Provider)
	}

	for _, ref := range []string{
		"Octo-Org/hello.world#7",
		"octo-org/HELLO.world#7", // GitHub names are case-insensitive
		url.PathEscape("Octo-Org/hello.world#7"),
		localKey, // the workspace key keeps working
	} {
		if byKey := getIssueForKeyTest(t, testHandler, ref); byKey.ID != issueID {
			t.Fatalf("GET %q resolved to %s, want %s", ref, byKey.ID, issueID)
		}
	}
	for _, ref := range []string{"Octo-Org/hello.world#8", "other/repo#7"} {
		testutil.Call(t, testHandler.GetIssue, withURLParam(newRequest(http.MethodGet, "/api/issues/x", nil), "id", ref)).
			Want(http.StatusNotFound)
	}
}

// The CLI sends the key path-escaped; the router matches on the escaped path,
// so the handler must accept the escapes it receives.
func TestIssueKey_GitHubKeyThroughRouter(t *testing.T) {
	setIssueKeysEnabledForTest(t, true)
	issueID := dbfx.Issue(t, "github issue via router")
	linkGitHubIssue(t, issueID, "octo/router", 3)

	router := chi.NewRouter()
	router.Get("/api/issues/{id}", testHandler.GetIssue)
	req := newRequest(http.MethodGet, "/api/issues/"+url.PathEscape("octo/router#3"), nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), issueID) {
		t.Fatalf("GET escaped key through router: %d %s", rec.Code, rec.Body.String())
	}
}

// countingDBTX records every statement the handler's queries run.
type countingDBTX struct {
	db.DBTX
	mu   sync.Mutex
	sqls []string
}

func (c *countingDBTX) record(sql string) {
	c.mu.Lock()
	c.sqls = append(c.sqls, sql)
	c.mu.Unlock()
}

func (c *countingDBTX) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	c.record(sql)
	return c.DBTX.Exec(ctx, sql, args...)
}

func (c *countingDBTX) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c.record(sql)
	return c.DBTX.Query(ctx, sql, args...)
}

func (c *countingDBTX) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	c.record(sql)
	return c.DBTX.QueryRow(ctx, sql, args...)
}

func (c *countingDBTX) reset() (total, external int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, sql := range c.sqls {
		if strings.Contains(sql, "issue_external") {
			external++
		}
	}
	total = len(c.sqls)
	c.sqls = nil
	return total, external
}

// The list endpoint reads issue_external once per page, however many issues
// the page holds and however many of them are GitHub-backed.
func TestIssueKey_ListReadsExternalsOncePerPage(t *testing.T) {
	setIssueKeysEnabledForTest(t, true)
	setWorkspaceIssuePrefixForTest(t, issueKeyTestPrefix)
	counter := &countingDBTX{DBTX: testPool}
	h := *testHandler
	h.Queries = db.New(counter)

	list := func(projectID string) []IssueResponse {
		var out struct {
			Issues []IssueResponse `json:"issues"`
		}
		testutil.Call(t, h.ListIssues, newRequest(http.MethodGet, "/api/issues?project_id="+projectID, nil)).
			Want(http.StatusOK).JSON(&out)
		return out.Issues
	}

	small := dbfx.Project(t, "issue key small page")
	dbfx.Issue(t, "small only", testutil.Cols{"project_id": small})
	counter.reset()
	if got := list(small); len(got) != 1 {
		t.Fatalf("small page: %d issues, want 1", len(got))
	}
	smallTotal, smallExternal := counter.reset()

	large := dbfx.Project(t, "issue key large page")
	local := dbfx.Issue(t, "large local", testutil.Cols{"project_id": large})
	gh1 := dbfx.Issue(t, "large github 1", testutil.Cols{"project_id": large})
	gh2 := dbfx.Issue(t, "large github 2", testutil.Cols{"project_id": large})
	linkGitHubIssue(t, gh1, "octo/list", 11)
	linkGitHubIssue(t, gh2, "octo/list", 12)
	counter.reset()
	got := list(large)
	largeTotal, largeExternal := counter.reset()

	if smallExternal != 1 || largeExternal != 1 {
		t.Fatalf("issue_external reads per list call: small=%d large=%d, want 1 each", smallExternal, largeExternal)
	}
	if largeTotal != smallTotal {
		t.Fatalf("queries per list call grew with the page: 1 issue=%d, 3 issues=%d", smallTotal, largeTotal)
	}

	byID := map[string]IssueResponse{}
	for _, issue := range got {
		byID[issue.ID] = issue
	}
	want := map[string][2]string{
		local: {issueKeyTestPrefix + "-" + strconv.Itoa(issueKeyTestNumber(t, local)), "local"},
		gh1:   {"octo/list#11", "github"},
		gh2:   {"octo/list#12", "github"},
	}
	for id, w := range want {
		if byID[id].Identifier != w[0] || byID[id].Provider != w[1] {
			t.Errorf("issue %s: identifier=%q provider=%q, want %q %q", id, byID[id].Identifier, byID[id].Provider, w[0], w[1])
		}
	}
}

func TestIssueKey_ChildrenAndDuplicateOf(t *testing.T) {
	setIssueKeysEnabledForTest(t, true)
	setWorkspaceIssuePrefixForTest(t, issueKeyTestPrefix)
	parent := dbfx.Issue(t, "parent")
	child := dbfx.Issue(t, "child", testutil.Cols{"parent_issue_id": parent})
	linkGitHubIssue(t, child, "octo/kids", 5)

	var children struct {
		Issues []IssueResponse `json:"issues"`
	}
	testutil.Call(t, testHandler.ListChildIssues, withURLParam(newRequest(http.MethodGet, "/api/issues/"+parent+"/children", nil), "id", parent)).
		Want(http.StatusOK).JSON(&children)
	if len(children.Issues) != 1 || children.Issues[0].Identifier != "octo/kids#5" || children.Issues[0].Provider != "github" {
		t.Fatalf("children: %+v", children.Issues)
	}

	original := dbfx.Issue(t, "original")
	linkGitHubIssue(t, original, "octo/dup", 9)
	duplicate := dbfx.Issue(t, "duplicate", testutil.Cols{"status": "cancelled", "duplicate_of_issue_id": original})
	got := getIssueForKeyTest(t, testHandler, duplicate)
	if got.Provider != "local" || got.DuplicateOf == nil || got.DuplicateOf.Identifier != "octo/dup#9" {
		t.Fatalf("duplicate: provider=%q duplicate_of=%+v, want local with octo/dup#9", got.Provider, got.DuplicateOf)
	}
}

// Events rendered outside the handler carry the same identifier and provider.
func TestIssueKey_IssueToMapResolved(t *testing.T) {
	setIssueKeysEnabledForTest(t, true)
	setWorkspaceIssuePrefixForTest(t, issueKeyTestPrefix)
	issueID := dbfx.Issue(t, "event issue")
	linkGitHubIssue(t, issueID, "octo/events", 21)
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
	if err != nil {
		t.Fatalf("load issue: %v", err)
	}
	m := service.IssueToMapResolved(context.Background(), testHandler.Queries, issue, issueKeyTestPrefix)
	if m["identifier"] != "octo/events#21" || m["provider"] != "github" {
		t.Fatalf("IssueToMapResolved: identifier=%v provider=%v", m["identifier"], m["provider"])
	}
	if key := testHandler.issueKey(context.Background(), issue); key != "octo/events#21" {
		t.Fatalf("issueKey: %q", key)
	}
	// The workflow brief tells the agent which key to pass back to the CLI.
	if in := testHandler.workflowBriefInput(context.Background(), issue, issueworkflow.Definition{}, db.Project{}); in.IssueIdentifier != "octo/events#21" {
		t.Fatalf("workflow brief identifier: %q", in.IssueIdentifier)
	}
}

func TestIssueKey_DeleteRemovesExternalRow(t *testing.T) {
	issueID := dbfx.Issue(t, "deleted github issue")
	linkGitHubIssue(t, issueID, "octo/gone", 1)

	testutil.Call(t, testHandler.DeleteIssue, withURLParam(newRequest(http.MethodDelete, "/api/issues/"+issueID, nil), "id", issueID)).
		Want(http.StatusNoContent)
	if n := dbfx.Count(t, `SELECT count(*) FROM issue_external WHERE issue_id = $1`, issueID); n != 0 {
		t.Fatalf("issue_external rows after delete: %d, want 0", n)
	}
}

// With the feature switch off, rows written while it was on are ignored:
// every surface renders the issue as local, no issue_external row is read,
// and the GitHub key does not resolve. The rows stay, so turning the switch
// back on restores the GitHub identifier.
func TestIssueKey_SwitchOffRendersExistingGitHubIssuesAsLocal(t *testing.T) {
	setWorkspaceIssuePrefixForTest(t, issueKeyTestPrefix)
	setIssueKeysEnabledForTest(t, false)
	project := dbfx.Project(t, "issue key switch off")
	issueID := dbfx.Issue(t, "synced before the switch went off", testutil.Cols{"project_id": project})
	linkGitHubIssue(t, issueID, "octo/off", 4)
	localKey := issueKeyTestPrefix + "-" + strconv.Itoa(issueKeyTestNumber(t, issueID))

	counter := &countingDBTX{DBTX: testPool}
	h := *testHandler
	h.Queries = db.New(counter)

	got := getIssueForKeyTest(t, &h, issueID)
	if got.Identifier != localKey || got.Provider != "local" {
		t.Fatalf("detail with switch off: identifier=%q provider=%q, want %q local", got.Identifier, got.Provider, localKey)
	}
	if byKey := getIssueForKeyTest(t, &h, localKey); byKey.ID != issueID {
		t.Fatalf("GET %s resolved to %s, want %s", localKey, byKey.ID, issueID)
	}
	testutil.Call(t, h.GetIssue, withURLParam(newRequest(http.MethodGet, "/api/issues/x", nil), "id", "octo/off#4")).
		Want(http.StatusNotFound)

	counter.reset()
	var list struct {
		Issues []IssueResponse `json:"issues"`
	}
	testutil.Call(t, h.ListIssues, newRequest(http.MethodGet, "/api/issues?project_id="+project, nil)).
		Want(http.StatusOK).JSON(&list)
	if len(list.Issues) != 1 || list.Issues[0].Identifier != localKey || list.Issues[0].Provider != "local" {
		t.Fatalf("list with switch off: %+v", list.Issues)
	}

	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
	if err != nil {
		t.Fatalf("load issue: %v", err)
	}
	m := service.IssueToMapResolved(context.Background(), h.Queries, issue, issueKeyTestPrefix)
	if m["identifier"] != localKey || m["provider"] != "local" {
		t.Fatalf("event with switch off: identifier=%v provider=%v", m["identifier"], m["provider"])
	}
	if in := h.workflowBriefInput(context.Background(), issue, issueworkflow.Definition{}, db.Project{}); in.IssueIdentifier != localKey {
		t.Fatalf("workflow brief with switch off: %q", in.IssueIdentifier)
	}
	if _, external := counter.reset(); external != 0 {
		t.Fatalf("issue_external read %d times with the switch off, want 0", external)
	}

	if n := dbfx.Count(t, `SELECT count(*) FROM issue_external WHERE issue_id = $1`, issueID); n != 1 {
		t.Fatalf("issue_external rows: %d, want the row kept", n)
	}
	setIssueKeysEnabledForTest(t, true)
	if got := getIssueForKeyTest(t, &h, issueID); got.Identifier != "octo/off#4" || got.Provider != "github" {
		t.Fatalf("after switching back on: identifier=%q provider=%q", got.Identifier, got.Provider)
	}
}
