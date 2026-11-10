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

func TestLoadAndKey(t *testing.T) {
	ws, local, gh := uuid(1), uuid(2), uuid(3)
	q := &fakeQuerier{rows: []db.IssueExternal{{IssueID: gh, Provider: ProviderGitHub, ExternalRepo: "octo/hello", ExternalNumber: 9}}}

	if got := Load(context.Background(), q, ws, nil); len(got) != 0 || q.calls != 0 {
		t.Fatalf("Load(no ids): %v after %d calls; want empty and no query", got, q.calls)
	}
	externals := Load(context.Background(), q, ws, []pgtype.UUID{local, gh})
	if q.calls != 1 {
		t.Fatalf("Load: %d queries, want 1", q.calls)
	}
	if got := externals.Key(local, "MUL-1"); got != "MUL-1" || externals.Provider(local) != ProviderLocal {
		t.Fatalf("local: %q %q", got, externals.Provider(local))
	}
	if got := externals.Key(gh, "MUL-2"); got != "octo/hello#9" || externals.Provider(gh) != ProviderGitHub {
		t.Fatalf("github: %q %q", got, externals.Provider(gh))
	}
	if got := LoadOne(context.Background(), q, ws, gh).Key(gh, "MUL-2"); got != "octo/hello#9" {
		t.Fatalf("LoadOne github: %q", got)
	}
	if got := LoadOne(context.Background(), q, ws, local).Key(local, "MUL-1"); got != "MUL-1" {
		t.Fatalf("LoadOne local: %q", got)
	}
}

func TestLoadFailureRendersLocal(t *testing.T) {
	q := &fakeQuerier{err: errors.New("boom")}
	externals := Load(context.Background(), q, uuid(1), []pgtype.UUID{uuid(2)})
	if got := externals.Key(uuid(2), "MUL-2"); got != "MUL-2" {
		t.Fatalf("Key after failed load = %q, want local", got)
	}
	if got := LoadOne(context.Background(), q, uuid(1), uuid(2)).Key(uuid(2), "MUL-2"); got != "MUL-2" {
		t.Fatalf("LoadOne after failure = %q, want local", got)
	}
}
