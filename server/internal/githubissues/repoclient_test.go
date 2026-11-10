package githubissues

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// fakeRepoStore stands in for *db.Queries: the registered resource refs of
// one workspace and the pins RepoClient writes.
type fakeRepoStore struct {
	mu      sync.Mutex
	refs    map[pgtype.UUID][][]byte
	pins    map[pgtype.UUID][]db.GithubRepoSync
	upserts int
}

func newFakeRepoStore() *fakeRepoStore {
	return &fakeRepoStore{refs: map[pgtype.UUID][][]byte{}, pins: map[pgtype.UUID][]db.GithubRepoSync{}}
}

func (f *fakeRepoStore) register(ws pgtype.UUID, url string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refs[ws] = append(f.refs[ws], []byte(`{"url":"`+url+`"}`))
}

func (f *fakeRepoStore) unregister(ws pgtype.UUID, url string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ref := `{"url":"` + url + `"}`
	kept := f.refs[ws][:0]
	for _, r := range f.refs[ws] {
		if string(r) != ref {
			kept = append(kept, r)
		}
	}
	f.refs[ws] = kept
}

func (f *fakeRepoStore) unregisterAll(ws pgtype.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.refs, ws)
}

func (f *fakeRepoStore) ListWorkspaceGitHubRepoRefs(_ context.Context, ws pgtype.UUID) ([][]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.refs[ws]...), nil
}

func (f *fakeRepoStore) ListGithubRepoSyncsByWorkspace(_ context.Context, ws pgtype.UUID) ([]db.GithubRepoSync, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]db.GithubRepoSync(nil), f.pins[ws]...), nil
}

func (f *fakeRepoStore) UpsertGithubRepoSync(_ context.Context, arg db.UpsertGithubRepoSyncParams) (db.GithubRepoSync, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upserts++
	row := db.GithubRepoSync{WorkspaceID: arg.WorkspaceID, RepoID: arg.RepoID, Owner: arg.Owner, Name: arg.Name}
	for i, pin := range f.pins[arg.WorkspaceID] {
		if pin.RepoID == arg.RepoID {
			f.pins[arg.WorkspaceID][i] = row
			return row, nil
		}
	}
	f.pins[arg.WorkspaceID] = append(f.pins[arg.WorkspaceID], row)
	return row, nil
}

// fakeRepoAPI is a GitHub API with repositories by name and by id. It
// records every request and fails the test if the token arrives anywhere but
// the Authorization header.
type fakeRepoAPI struct {
	t      *testing.T
	server *httptest.Server
	mu     sync.Mutex
	// byName maps "owner/name" (lowercase) to the repository id GitHub
	// currently gives that name; a negative id redirects the name to
	// /repositories/{-id}, as GitHub does after a rename.
	byName   map[string]int64
	requests []string
	respond  func(w http.ResponseWriter, r *http.Request) bool
}

func newFakeRepoAPI(t *testing.T) *fakeRepoAPI {
	t.Helper()
	f := &fakeRepoAPI{t: t, byName: map[string]int64{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeRepoAPI) setName(fullName string, id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byName[strings.ToLower(fullName)] = id
}

func (f *fakeRepoAPI) setResponder(respond func(w http.ResponseWriter, r *http.Request) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.respond = respond
}

func (f *fakeRepoAPI) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func (f *fakeRepoAPI) serve(w http.ResponseWriter, r *http.Request) {
	if got := r.Header.Get("Authorization"); got != "Bearer "+fakeToken {
		f.t.Errorf("Authorization = %q, want the bearer token", got)
	}
	if strings.Contains(r.URL.String(), fakeToken[:12]) {
		f.t.Errorf("token leaked into the URL")
	}
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
	respond := f.respond
	f.mu.Unlock()
	if respond != nil && respond(w, r) {
		return
	}

	if rest, ok := strings.CutPrefix(r.URL.Path, "/repos/"); ok {
		f.mu.Lock()
		id, found := f.byName[strings.ToLower(rest)]
		f.mu.Unlock()
		switch {
		case !found:
			http.NotFound(w, r)
		case id < 0:
			http.Redirect(w, r, fmt.Sprintf("/repositories/%d", -id), http.StatusMovedPermanently)
		default:
			_, _ = fmt.Fprintf(w, `{"id":%d,"full_name":%q}`, id, rest)
		}
		return
	}
	if rest, ok := strings.CutPrefix(r.URL.Path, "/repositories/"); ok {
		id, _, _ := strings.Cut(rest, "/")
		_, _ = fmt.Fprintf(w, `{"id":%s,"path":%q}`, id, r.URL.Path)
		return
	}
	http.NotFound(w, r)
}

type repoFixture struct {
	testService
	api   *fakeRepoAPI
	store *fakeRepoStore
	ws    pgtype.UUID
}

func newRepoFixture(t *testing.T) repoFixture {
	t.Helper()
	api := newFakeRepoAPI(t)
	svc := newTestService(t, api.server.URL, "2027-03-01")
	store := newFakeRepoStore()
	svc.store = store
	svc.state.markAccepted("octocat", testNow)
	return repoFixture{testService: svc, api: api, store: store, ws: testUUID(1)}
}

func testUUID(b byte) pgtype.UUID {
	return pgtype.UUID{Bytes: [16]byte{15: b}, Valid: true}
}

func TestRepoClientRejectsUnregisteredWithoutRequest(t *testing.T) {
	fx := newRepoFixture(t)
	fx.store.register(fx.ws, "https://github.com/acme/widgets")
	fx.store.register(fx.ws, "https://gitlab.com/acme/elsewhere")
	fx.store.register(testUUID(2), "https://github.com/acme/other-workspace")
	fx.api.setName("acme/elsewhere", 7)
	fx.api.setName("acme/other-workspace", 8)

	for _, tc := range []struct{ owner, repo string }{
		{"acme", "gadgets"},               // not registered anywhere
		{"acme", "elsewhere"},             // registered, but not on github.com
		{"acme", "other-workspace"},       // registered in another workspace
		{"acme", "widgets/../../gadgets"}, // not a repository name
		{"..", "widgets"},
		{"", ""},
	} {
		_, err := fx.RepoClient(context.Background(), fx.ws, tc.owner, tc.repo)
		if !errors.Is(err, ErrRepoNotRegistered) {
			t.Errorf("%s/%s: err = %v, want ErrRepoNotRegistered", tc.owner, tc.repo, err)
		}
	}
	if got := fx.api.seen(); len(got) != 0 {
		t.Fatalf("unregistered repositories must not reach GitHub, saw %v", got)
	}
	if fx.store.upserts != 0 {
		t.Fatalf("unregistered repositories must not be pinned")
	}
}

func TestRepoClientOnNilServiceIsOff(t *testing.T) {
	var s *Service
	if _, err := s.RepoClient(context.Background(), testUUID(1), "acme", "widgets"); !errors.Is(err, ErrFeatureOff) {
		t.Fatalf("err = %v, want ErrFeatureOff", err)
	}
}

func TestRepoClientNeedsUsableToken(t *testing.T) {
	fx := newRepoFixture(t)
	fx.store.register(fx.ws, "https://github.com/acme/widgets")
	fx.state.markRejected(testNow)
	if _, err := fx.RepoClient(context.Background(), fx.ws, "acme", "widgets"); !errors.Is(err, ErrTokenNotUsable) {
		t.Fatalf("err = %v, want ErrTokenNotUsable", err)
	}

	expired := newRepoFixture(t)
	expired.store.register(expired.ws, "https://github.com/acme/widgets")
	*expired.now = testNow.AddDate(1, 0, 0)
	if _, err := expired.RepoClient(context.Background(), expired.ws, "acme", "widgets"); !errors.Is(err, ErrTokenNotUsable) {
		t.Fatalf("expired token: err = %v, want ErrTokenNotUsable", err)
	}
	if got := append(fx.api.seen(), expired.api.seen()...); len(got) != 0 {
		t.Fatalf("an unusable token must not reach GitHub, saw %v", got)
	}
}

func TestRepoClientPinsIDAndAddressesRepositoryByID(t *testing.T) {
	fx := newRepoFixture(t)
	fx.store.register(fx.ws, "git@github.com:Acme/Widgets.git")
	fx.api.setName("acme/widgets", 42)
	ctx := context.Background()

	repo, err := fx.RepoClient(ctx, fx.ws, "acme", "widgets")
	if err != nil {
		t.Fatal(err)
	}
	if repo.ID() != 42 || repo.FullName() != "acme/widgets" {
		t.Fatalf("repo = %v", repo)
	}
	pins, _ := fx.store.ListGithubRepoSyncsByWorkspace(ctx, fx.ws)
	if len(pins) != 1 || pins[0].RepoID != 42 {
		t.Fatalf("pins = %+v", pins)
	}

	resp, err := repo.Do(ctx, http.MethodGet, "/issues?state=all&since=2027-01-01T00:00:00Z", nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("Do: %v %+v", err, resp)
	}
	// A second client for the same name uses the pin: no lookup by name.
	again, err := fx.RepoClient(ctx, fx.ws, "ACME", "widgets")
	if err != nil || again.ID() != 42 {
		t.Fatalf("second client: %v %v", again, err)
	}
	want := []string{
		"GET /repos/acme/widgets",
		"GET /repositories/42/issues?state=all&since=2027-01-01T00:00:00Z",
	}
	if got := fx.api.seen(); !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
}

func TestRepoClientKeepsRepoIDAcrossRename(t *testing.T) {
	fx := newRepoFixture(t)
	ctx := context.Background()
	fx.store.register(fx.ws, "https://github.com/acme/widgets")
	fx.api.setName("acme/widgets", 42)
	if _, err := fx.RepoClient(ctx, fx.ws, "acme", "widgets"); err != nil {
		t.Fatal(err)
	}

	// acme/widgets is renamed to acme/gadgets and someone creates a new
	// acme/widgets (id 99). The registered old name keeps reaching 42.
	fx.api.setName("acme/gadgets", 42)
	fx.api.setName("acme/widgets", 99)
	repo, err := fx.RepoClient(ctx, fx.ws, "acme", "widgets")
	if err != nil || repo.ID() != 42 {
		t.Fatalf("old name after rename: %v %v", repo, err)
	}
	if _, err := repo.Do(ctx, http.MethodGet, "/issues/1", nil); err != nil {
		t.Fatal(err)
	}

	// The new name is not registered yet, so it is refused even though it is
	// the same repository.
	if _, err := fx.RepoClient(ctx, fx.ws, "acme", "gadgets"); !errors.Is(err, ErrRepoNotRegistered) {
		t.Fatalf("unregistered new name: err = %v", err)
	}

	// The Owner updates the resource to the new name: it resolves to the
	// same id, which keeps its pin row (and its poll cursors).
	fx.store.unregisterAll(fx.ws)
	fx.store.register(fx.ws, "https://github.com/acme/gadgets")
	repo, err = fx.RepoClient(ctx, fx.ws, "acme", "gadgets")
	if err != nil || repo.ID() != 42 {
		t.Fatalf("new name: %v %v", repo, err)
	}
	pins, _ := fx.store.ListGithubRepoSyncsByWorkspace(ctx, fx.ws)
	if len(pins) != 1 || pins[0].RepoID != 42 || pins[0].Name != "gadgets" {
		t.Fatalf("pins = %+v, want one row for 42 named gadgets", pins)
	}

	want := []string{
		"GET /repos/acme/widgets",
		"GET /repositories/42/issues/1",
		"GET /repos/acme/gadgets",
	}
	if got := fx.api.seen(); !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
}

// Review R1: project A keeps the old name, project B registers the new one.
// The old name's pin must survive any order of calls; the new name, which
// GitHub resolves to the same repository, is refused instead of taking the
// pin over (taking it over sent the old name to the squatter, id 99).
func TestRepoClientKeepsPinWhileBothNamesAreRegistered(t *testing.T) {
	fx := newRepoFixture(t)
	ctx := context.Background()
	fx.store.register(fx.ws, "https://github.com/acme/widgets") // project A
	fx.api.setName("acme/widgets", 42)
	if repo, err := fx.RepoClient(ctx, fx.ws, "acme", "widgets"); err != nil || repo.ID() != 42 {
		t.Fatalf("pin old name: %v %v", repo, err)
	}

	fx.api.setName("acme/gadgets", 42)                          // renamed
	fx.api.setName("acme/widgets", 99)                          // old name taken by another repository
	fx.store.register(fx.ws, "https://github.com/acme/gadgets") // project B

	for round := range 3 {
		if _, err := fx.RepoClient(ctx, fx.ws, "acme", "gadgets"); !errors.Is(err, ErrRepoAmbiguous) {
			t.Fatalf("round %d, new name: err = %v, want ErrRepoAmbiguous", round, err)
		}
		repo, err := fx.RepoClient(ctx, fx.ws, "acme", "widgets")
		if err != nil || repo.ID() != 42 {
			t.Fatalf("round %d, old name: %v %v, want 42", round, repo, err)
		}
		if _, err := repo.Do(ctx, http.MethodGet, "/issues", nil); err != nil {
			t.Fatal(err)
		}
	}
	pins, _ := fx.store.ListGithubRepoSyncsByWorkspace(ctx, fx.ws)
	if len(pins) != 1 || pins[0].RepoID != 42 || pins[0].Name != "widgets" {
		t.Fatalf("pins = %+v, want only 42 still named widgets", pins)
	}
	for _, req := range fx.api.seen() {
		if strings.Contains(req, "/repositories/99") {
			t.Fatalf("reached the repository squatting on the old name: %s", req)
		}
	}

	// Project A drops the old name: the new name takes the pin over.
	fx.store.unregister(fx.ws, "https://github.com/acme/widgets")
	repo, err := fx.RepoClient(ctx, fx.ws, "acme", "gadgets")
	if err != nil || repo.ID() != 42 {
		t.Fatalf("new name after the old one is gone: %v %v", repo, err)
	}
	pins, _ = fx.store.ListGithubRepoSyncsByWorkspace(ctx, fx.ws)
	if len(pins) != 1 || pins[0].RepoID != 42 || pins[0].Name != "gadgets" {
		t.Fatalf("pins = %+v, want 42 renamed to gadgets", pins)
	}
}

// The reverse order: the new name is pinned first, then a project registers
// the old name, which GitHub still redirects to the same repository.
func TestRepoClientRefusesOldNameRedirectingToAPinnedRepository(t *testing.T) {
	fx := newRepoFixture(t)
	ctx := context.Background()
	fx.store.register(fx.ws, "https://github.com/acme/gadgets")
	fx.api.setName("acme/gadgets", 42)
	fx.api.setName("acme/widgets", -42)
	if _, err := fx.RepoClient(ctx, fx.ws, "acme", "gadgets"); err != nil {
		t.Fatal(err)
	}
	fx.store.register(fx.ws, "https://github.com/acme/widgets")
	if _, err := fx.RepoClient(ctx, fx.ws, "acme", "widgets"); !errors.Is(err, ErrRepoAmbiguous) {
		t.Fatalf("old name: err = %v, want ErrRepoAmbiguous", err)
	}
	repo, err := fx.RepoClient(ctx, fx.ws, "acme", "gadgets")
	if err != nil || repo.ID() != 42 {
		t.Fatalf("pinned name: %v %v", repo, err)
	}
	pins, _ := fx.store.ListGithubRepoSyncsByWorkspace(ctx, fx.ws)
	if len(pins) != 1 || pins[0].Name != "gadgets" {
		t.Fatalf("pins = %+v", pins)
	}
}

func TestRepoClientFollowsRenameRedirectWhenPinning(t *testing.T) {
	fx := newRepoFixture(t)
	fx.store.register(fx.ws, "https://github.com/acme/widgets")
	fx.api.setName("acme/widgets", -42) // renamed before it was ever pinned
	repo, err := fx.RepoClient(context.Background(), fx.ws, "acme", "widgets")
	if err != nil || repo.ID() != 42 {
		t.Fatalf("repo = %v, err = %v", repo, err)
	}
}

func TestRepoClientPinFailures(t *testing.T) {
	fx := newRepoFixture(t)
	fx.store.register(fx.ws, "https://github.com/acme/missing")
	if _, err := fx.RepoClient(context.Background(), fx.ws, "acme", "missing"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("missing repository: err = %v", err)
	}
	if fx.store.upserts != 0 {
		t.Fatal("a failed lookup must not pin anything")
	}
}

func TestRepoDoRefusesPathsOutsideTheRepository(t *testing.T) {
	fx := newRepoFixture(t)
	repo := &Repo{svc: fx.Service, workspaceID: fx.ws, id: 42, name: repoName{"acme", "widgets"}}
	for _, path := range []string{
		"issues",
		"//evil.example/x",
		"/../../user",
		"/issues/../../../user",
		"/%2e%2e/%2e%2e/user",
		"/./issues",
		`/issues\..\user`,
		"/issues#frag",
		"https://evil.example/x",
	} {
		if _, err := repo.Do(context.Background(), http.MethodGet, path, nil); err == nil {
			t.Errorf("path %q was accepted", path)
		}
	}
	if _, err := repo.Do(context.Background(), "TRACE", "/issues", nil); err == nil {
		t.Error("TRACE was accepted")
	}
	if got := fx.api.seen(); len(got) != 0 {
		t.Fatalf("refused paths must not reach GitHub, saw %v", got)
	}
}

func TestRepoDoSendsBodyAndDoesNotFollowRedirects(t *testing.T) {
	fx := newRepoFixture(t)
	repo := &Repo{svc: fx.Service, workspaceID: fx.ws, id: 42, name: repoName{"acme", "widgets"}}
	var gotBody, gotType string
	fx.api.setResponder(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost {
			return false
		}
		body, _ := io.ReadAll(r.Body)
		gotBody, gotType = string(body), r.Header.Get("Content-Type")
		http.Redirect(w, r, "/user", http.StatusTemporaryRedirect)
		return true
	})
	resp, err := repo.Do(context.Background(), http.MethodPost, "/issues/1/comments", []byte(`{"body":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want the redirect handed back", resp.StatusCode)
	}
	if gotBody != `{"body":"hi"}` || gotType != "application/json" {
		t.Fatalf("body %q, content type %q", gotBody, gotType)
	}
	if got := fx.api.seen(); len(got) != 1 {
		t.Fatalf("redirect was followed: %v", got)
	}
}

func TestRepoDoUnauthorizedTurnsTokenInvalid(t *testing.T) {
	fx := newRepoFixture(t)
	repo := &Repo{svc: fx.Service, workspaceID: fx.ws, id: 42, name: repoName{"acme", "widgets"}}
	fx.api.setResponder(func(w http.ResponseWriter, _ *http.Request) bool {
		failure(http.StatusUnauthorized, nil, "Bad credentials")(w)
		return true
	})
	_, err := repo.Do(context.Background(), http.MethodGet, "/issues", nil)
	if !errors.Is(err, ErrTokenNotUsable) {
		t.Fatalf("err = %v, want ErrTokenNotUsable", err)
	}
	assertNoToken(t, "error", err.Error())
	if fx.Usable() || fx.TokenStatus().Status != TokenInvalid {
		t.Fatalf("a 401 must turn the token invalid, status %+v", fx.TokenStatus())
	}
	if _, err := repo.Do(context.Background(), http.MethodGet, "/issues", nil); !errors.Is(err, ErrTokenNotUsable) {
		t.Fatalf("after 401: err = %v", err)
	}
	if got := fx.api.seen(); len(got) != 1 {
		t.Fatalf("an invalid token must not be sent again, saw %v", got)
	}
}

func TestRepoDoRateLimitAndOtherStatuses(t *testing.T) {
	fx := newRepoFixture(t)
	repo := &Repo{svc: fx.Service, workspaceID: fx.ws, id: 42, name: repoName{"acme", "widgets"}}
	fx.api.setResponder(func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/repositories/42/limited":
			failure(http.StatusTooManyRequests, map[string]string{"Retry-After": "30"}, "slow down")(w)
		case "/repositories/42/forbidden":
			failure(http.StatusForbidden, nil, "Resource protected by organization SAML enforcement")(w)
		default:
			return false
		}
		return true
	})
	_, err := repo.Do(context.Background(), http.MethodGet, "/limited", nil)
	var limited *ghsnapshot.RateLimitError
	if !errors.As(err, &limited) {
		t.Fatalf("err = %v, want a rate limit", err)
	}
	resp, err := repo.Do(context.Background(), http.MethodGet, "/forbidden", nil)
	if err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("403: %v %+v", err, resp)
	}
	if !fx.Usable() {
		t.Fatal("a per-repository 403 must not invalidate the token")
	}
}

func TestRepoAndServiceExposeNoTokenOrClient(t *testing.T) {
	fx := newRepoFixture(t)
	repo := &Repo{svc: fx.Service, workspaceID: fx.ws, id: 42, name: repoName{"acme", "widgets"}}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%d", "%x"} {
		assertNoToken(t, "Repo "+verb, fmt.Sprintf(verb, repo))
		assertNoToken(t, "Repo value "+verb, fmt.Sprintf(verb, *repo))
		assertNoToken(t, "Service "+verb, fmt.Sprintf(verb, fx.Service))
		assertNoToken(t, "nested "+verb, fmt.Sprintf(verb, struct{ R *Repo }{repo}))
	}
	var logs strings.Builder
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("x", "repo", repo, "svc", fx.Service)
	assertNoToken(t, "slog", logs.String())
	if !strings.Contains(logs.String(), `"repo_id":42`) {
		t.Fatalf("slog output lacks the repository: %s", logs.String())
	}

	forbidden := map[reflect.Type]bool{
		reflect.TypeOf((*http.Client)(nil)):              true,
		reflect.TypeOf((*http.Request)(nil)):             true,
		reflect.TypeOf((*http.RoundTripper)(nil)).Elem(): true,
		reflect.TypeOf(http.Header(nil)):                 false, // Response.Header is fine
	}
	for _, typ := range []reflect.Type{reflect.TypeOf(fx.Service), reflect.TypeOf(repo)} {
		for i := range typ.NumMethod() {
			m := typ.Method(i)
			for j := range m.Type.NumOut() {
				if forbidden[m.Type.Out(j)] {
					t.Errorf("%s.%s returns %s", typ, m.Name, m.Type.Out(j))
				}
			}
		}
		if typ.Elem().Kind() == reflect.Struct {
			for i := range typ.Elem().NumField() {
				if typ.Elem().Field(i).IsExported() {
					t.Errorf("%s has exported field %s", typ, typ.Elem().Field(i).Name)
				}
			}
		}
	}
}

func TestRepoNameFromResourceRef(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want string
	}{
		{"https://github.com/acme/widgets", "acme/widgets"},
		{"https://github.com/acme/widgets.git", "acme/widgets"},
		{"https://github.com/acme/widgets/", "acme/widgets"},
		{"http://www.github.com/acme/widgets", "acme/widgets"},
		{"https://GitHub.com/Acme/Widgets", "Acme/Widgets"},
		{"ssh://git@github.com/acme/widgets.git", "acme/widgets"},
		{"git@github.com:acme/widgets.git", "acme/widgets"},
		{"github.com:acme/widgets", "acme/widgets"},
		{"https://github.com/acme/my.repo_1-x", "acme/my.repo_1-x"},
		{"https://gitlab.com/acme/widgets", ""},
		{"https://github.example.com/acme/widgets", ""},
		{"https://github.com.evil.example/acme/widgets", ""},
		{"https://github.com:8443/acme/widgets", ""},
		{"https://github.com/acme", ""},
		{"https://github.com/acme/widgets/tree/main", ""},
		{"https://github.com/acme/widgets?x=1", ""},
		{"https://github.com/acme/..", ""},
		{"https://github.com/acme/%2e%2e", ""},
		{"git@gitlab.com:acme/widgets.git", ""},
		{"ftp://github.com/acme/widgets", ""},
		{"not a url", ""},
		{"", ""},
	} {
		ref := []byte(`{"url":"` + tc.url + `"}`)
		got, ok := repoNameFromResourceRef(ref)
		if tc.want == "" {
			if ok {
				t.Errorf("%q: got %s, want rejected", tc.url, got)
			}
			continue
		}
		if !ok || got.String() != tc.want {
			t.Errorf("%q: got %v %v, want %s", tc.url, got, ok, tc.want)
		}
	}
	if _, ok := repoNameFromResourceRef([]byte(`not json`)); ok {
		t.Error("malformed resource_ref accepted")
	}
}
