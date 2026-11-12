package githubissues

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/migrations"
)

// The fork's migration numbering rule, documented in docs/github-issues.md:
// fork migrations take 9000–9999 and are named 9NNN_<feature>_<desc>, where
// <feature> is the fork package the migration belongs to.
const (
	forkMigrationFirst = 9000
	forkMigrationLast  = 9999
)

var (
	forkMigrationStem    = regexp.MustCompile(`^9[0-9]{3}_(githubissues|passwordauth)_[a-z0-9_]+$`)
	forkMigrationFeature = regexp.MustCompile(`githubissues|passwordauth`)
)

func migrationNumber(stem string) (int, bool) {
	prefix, _, ok := strings.Cut(stem, "_")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(prefix)
	return n, err == nil
}

func realMigrationStems(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no migration files found in server/migrations")
	}
	var stems []string
	for _, f := range files {
		stems = append(stems, migrations.ExtractVersion(f))
	}
	return stems
}

func TestForkMigrationsFollowNumberingRule(t *testing.T) {
	for _, stem := range realMigrationStems(t) {
		n, ok := migrationNumber(stem)
		inRange := ok && n >= forkMigrationFirst && n <= forkMigrationLast
		isFork := forkMigrationFeature.MatchString(stem)
		switch {
		case isFork && !inRange:
			t.Errorf("%s: fork migrations must be numbered %d-%d", stem, forkMigrationFirst, forkMigrationLast)
		case inRange && !forkMigrationStem.MatchString(stem):
			t.Errorf("%s: numbers %d-%d are reserved for fork migrations named 9NNN_<feature>_<desc>; "+
				"if upstream reached this range, revisit the rule in docs/github-issues.md",
				stem, forkMigrationFirst, forkMigrationLast)
		case ok && n > forkMigrationLast:
			t.Errorf("%s: number above the fork range; revisit the rule in docs/github-issues.md", stem)
		}
	}
}

// The runner applies files in migrations.Files order, which is a string sort
// of the paths, not a numeric one. This runs the real Files / AllVersions
// against today's upstream file names plus hypothetical fork migrations and
// checks the properties the numbering rule relies on.
func TestForkMigrationRangeOrdering(t *testing.T) {
	dir := t.TempDir()
	migrationsDir := filepath.Join(dir, "migrations")
	if err := os.Mkdir(migrationsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var upstream []string
	for _, stem := range realMigrationStems(t) {
		if forkMigrationStem.MatchString(stem) {
			continue
		}
		upstream = append(upstream, stem)
	}
	fork := []string{
		"9000_githubissues_first",
		"9001_githubissues_first_idx",
		"9002_passwordauth_second",
		"9010_githubissues_tenth",
	}
	for _, stem := range append(slices.Clone(upstream), fork...) {
		for _, direction := range []string{"up", "down"} {
			if err := os.WriteFile(filepath.Join(migrationsDir, stem+"."+direction+".sql"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Chdir(dir)

	versions, err := migrations.AllVersions()
	if err != nil {
		t.Fatal(err)
	}
	position := make(map[string]int, len(versions))
	for i, v := range versions {
		position[v] = i
	}

	// Every fork migration is applied, in its own numeric order.
	for i, stem := range fork {
		if _, ok := position[stem]; !ok {
			t.Fatalf("%s missing from AllVersions", stem)
		}
		if i > 0 && position[fork[i-1]] > position[stem] {
			t.Errorf("up order: %s runs before %s", stem, fork[i-1])
		}
	}

	// Fork migrations run after every upstream migration numbered below 900,
	// which covers every upstream table they can reference. Upstream 900-999
	// would sort after them ("9000_" < "900_" because '0' < '_'); the fork
	// creates only its own tables, so that order does not matter.
	firstFork := position[fork[0]]
	for _, stem := range upstream {
		if n, ok := migrationNumber(stem); ok && n < 900 && position[stem] > firstFork {
			t.Errorf("up order: upstream %s runs after fork %s", stem, fork[0])
		}
	}

	// Rollback walks the same list in reverse.
	down, err := migrations.Files("down")
	if err != nil {
		t.Fatal(err)
	}
	var downFork []string
	for _, f := range down {
		if stem := migrations.ExtractVersion(f); slices.Contains(fork, stem) {
			downFork = append(downFork, stem)
		}
	}
	want := slices.Clone(fork)
	slices.Reverse(want)
	if !slices.Equal(downFork, want) {
		t.Errorf("down order of fork migrations = %v, want %v", downFork, want)
	}
}
