package issuekey

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestParseGitHub(t *testing.T) {
	valid := map[string]struct {
		repo   string
		number int32
	}{
		"octo/hello#1":                {"octo/hello", 1},
		"Octo-Org/hello.world_x-y#42": {"Octo-Org/hello.world_x-y", 42},
		"a/b#2147483647":              {"a/b", 2147483647},
	}
	for in, want := range valid {
		repo, number, ok := ParseGitHub(in)
		if !ok || repo != want.repo || number != want.number {
			t.Errorf("ParseGitHub(%q) = %q, %d, %v; want %q, %d, true", in, repo, number, ok, want.repo, want.number)
		}
	}
	for _, in := range []string{
		"", "MUL-42", "octo/hello", "octo/hello#", "octo/hello#0", "octo/hello#-1",
		"octo/hello#2147483648", "octo#1", "/hello#1", "octo/#1", "-octo/hello#1",
		"octo/hel lo#1", "octo/hello/x#1", "octo/hello#1x", " octo/hello#1",
	} {
		if repo, number, ok := ParseGitHub(in); ok {
			t.Errorf("ParseGitHub(%q) = %q, %d, true; want not ok", in, repo, number)
		}
	}
}

func TestGitHubRoundTrip(t *testing.T) {
	key := GitHub(db.IssueExternal{ExternalRepo: "octo/hello", ExternalNumber: 7})
	if key != "octo/hello#7" {
		t.Fatalf("GitHub() = %q", key)
	}
	if repo, number, ok := ParseGitHub(key); !ok || repo != "octo/hello" || number != 7 {
		t.Fatalf("ParseGitHub(GitHub()) = %q, %d, %v", repo, number, ok)
	}
	if url := URL(db.IssueExternal{ExternalRepo: "octo/hello", ExternalNumber: 7}); url != "https://github.com/octo/hello/issues/7" {
		t.Fatalf("URL() = %q", url)
	}
}

type fakeQuerier struct {
	rows  []db.IssueExternal
	err   error
	calls int
}

func (f *fakeQuerier) GetIssueExternal(_ context.Context, arg db.GetIssueExternalParams) (db.IssueExternal, error) {
	f.calls++
	if f.err != nil {
		return db.IssueExternal{}, f.err
	}
	for _, row := range f.rows {
		if row.IssueID == arg.IssueID {
			return row, nil
		}
	}
	return db.IssueExternal{}, pgx.ErrNoRows
}

func (f *fakeQuerier) ListIssueExternalsByIssueIDs(_ context.Context, _ db.ListIssueExternalsByIssueIDsParams) ([]db.IssueExternal, error) {
	f.calls++
	return f.rows, f.err
}

func uuid(b byte) pgtype.UUID { return pgtype.UUID{Bytes: [16]byte{b}, Valid: true} }

func enableForTest(t *testing.T) {
	t.Helper()
	previous := Enabled()
	SetEnabled(true)
	t.Cleanup(func() { SetEnabled(previous) })
}

func TestLoadAndKey(t *testing.T) {
	enableForTest(t)
	ws, local, gh := uuid(1), uuid(2), uuid(3)
	q := &fakeQuerier{rows: []db.IssueExternal{{IssueID: gh, Provider: ProviderGitHub, ExternalRepo: "octo/hello", ExternalNumber: 9}}}

	if got := Load(context.Background(), q, ws, nil); len(got) != 0 || q.calls != 0 {
		t.Fatalf("Load(no ids): %v after %d calls; want empty and no query", got, q.calls)
	}
	externals := Load(context.Background(), q, ws, []pgtype.UUID{local, gh})
	if q.calls != 1 {
		t.Fatalf("Load: %d queries, want 1", q.calls)
	}
	if _, ok := externals[local]; ok || externals.Provider(local) != ProviderLocal {
		t.Fatalf("local: %v %q", externals[local], externals.Provider(local))
	}
	if ext := externals[gh]; ext.ExternalRepo != "octo/hello" || ext.ExternalNumber != 9 || externals.Provider(gh) != ProviderGitHub {
		t.Fatalf("github: %+v %q", ext, externals.Provider(gh))
	}
	if one := LoadOne(context.Background(), q, ws, gh); one[gh].ExternalNumber != 9 || one.Provider(gh) != ProviderGitHub {
		t.Fatalf("LoadOne github: %+v", one)
	}
	if one := LoadOne(context.Background(), q, ws, local); len(one) != 0 || one.Provider(local) != ProviderLocal {
		t.Fatalf("LoadOne local: %+v", one)
	}
}

func TestLoadFailureRendersLocal(t *testing.T) {
	enableForTest(t)
	q := &fakeQuerier{err: errors.New("boom")}
	externals := Load(context.Background(), q, uuid(1), []pgtype.UUID{uuid(2)})
	if got := externals.Provider(uuid(2)); got != ProviderLocal {
		t.Fatalf("Provider after failed load = %q, want local", got)
	}
	if got := LoadOne(context.Background(), q, uuid(1), uuid(2)).Provider(uuid(2)); got != ProviderLocal {
		t.Fatalf("LoadOne after failure = %q, want local", got)
	}
}

func TestSwitchOffSkipsReads(t *testing.T) {
	SetEnabled(false)
	gh := uuid(3)
	q := &fakeQuerier{rows: []db.IssueExternal{{IssueID: gh, Provider: ProviderGitHub, ExternalRepo: "octo/hello", ExternalNumber: 9}}}
	if got := Load(context.Background(), q, uuid(1), []pgtype.UUID{gh}).Provider(gh); got != ProviderLocal {
		t.Fatalf("Load with switch off: %q, want local", got)
	}
	if got := LoadOne(context.Background(), q, uuid(1), gh).Provider(gh); got != ProviderLocal {
		t.Fatalf("LoadOne with switch off: provider %q, want local", got)
	}
	if q.calls != 0 {
		t.Fatalf("switch off made %d queries, want 0", q.calls)
	}
}
