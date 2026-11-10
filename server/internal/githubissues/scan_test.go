package githubissues

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// forkMarker is the modification notice the fork puts on every upstream file
// it changes (LICENSE). It is how the scan finds the ghsnapshot files the fork
// touched, including ones changed by later tasks.
const forkMarker = "mouse9527/multica fork"

// The only files allowed to do what the scan otherwise forbids.
const (
	transportFile = "transport.go" // builds requests, reads the token
	configFile    = "config.go"    // loads the token from the environment
)

// bypassRule is one way around RepoClient. allowedIn names the githubissues
// file that may do it; fork-changed ghsnapshot files may do none of them.
type bypassRule struct {
	what      string
	allowedIn string
	match     func(n ast.Node) bool
}

func selectorOf(n ast.Node, pkg string, names ...string) bool {
	sel, ok := n.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if pkg != "" {
		if x, ok := sel.X.(*ast.Ident); !ok || x.Name != pkg {
			return false
		}
	}
	for _, name := range names {
		if sel.Sel.Name == name {
			return true
		}
	}
	return false
}

func stringLiteral(n ast.Node, match func(string) bool) bool {
	lit, ok := n.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	value, err := strconv.Unquote(lit.Value)
	return err == nil && match(value)
}

var bypassRules = []bypassRule{
	{
		what: "http.NewRequest", allowedIn: transportFile,
		match: func(n ast.Node) bool { return selectorOf(n, "http", "NewRequest", "NewRequestWithContext") },
	},
	{
		what: "default net/http client",
		match: func(n ast.Node) bool {
			return selectorOf(n, "http", "Get", "Head", "Post", "PostForm", "DefaultClient", "DefaultTransport")
		},
	},
	{
		what: "token field read", allowedIn: transportFile,
		match: func(n ast.Node) bool { return selectorOf(n, "", "token") },
	},
	{
		what: "Authorization header", allowedIn: transportFile,
		match: func(n ast.Node) bool {
			return stringLiteral(n, func(s string) bool { return strings.EqualFold(s, "Authorization") })
		},
	},
	{
		// Reading the variable takes a call with its name as an argument;
		// naming it in a message (fmt) is not a read.
		what: "token environment variable", allowedIn: configFile,
		match: func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || selectorOf(call.Fun, "fmt", "Sprintf", "Sprint", "Errorf", "Fprintf") {
				return false
			}
			for _, arg := range call.Args {
				if id, ok := arg.(*ast.Ident); ok && id.Name == "EnvToken" {
					return true
				}
				if selectorOf(arg, "", "EnvToken") || stringLiteral(arg, func(s string) bool { return s == EnvToken }) {
					return true
				}
			}
			return false
		},
	},
	{
		what: "environment read",
		match: func(n ast.Node) bool {
			return selectorOf(n, "os", "Getenv", "LookupEnv", "Environ") || selectorOf(n, "syscall", "Getenv", "Environ")
		},
	},
	{
		what: "raw client Do", allowedIn: transportFile,
		match: func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Do" {
				return false
			}
			inner, ok := sel.X.(*ast.SelectorExpr)
			return ok && strings.HasSuffix(strings.ToLower(inner.Sel.Name), "httpclient")
		},
	},
}

// upstreamAppPath lists, per upstream ghsnapshot file the fork changed, the
// upstream functions that authenticate as the GitHub App, with the rules each
// may break to do so. They mint and send App installation tokens, never the
// GitHub issues token. Everything else in these files is scanned in full,
// and the static source's requests are covered by TestSnapshotTransport*.
var upstreamAppPath = map[string]map[string][]string{
	"client.go": {
		"NewClientFromEnv":      {"environment read"},
		"cachedToken":           {"token field read"},
		"mintInstallationToken": {"http.NewRequest", "Authorization header", "raw client Do"},
		// Sends both sources' requests; only App requests carry a token here.
		"restGet": {"http.NewRequest", "Authorization header"},
	},
}

// scanForBypass returns one finding per forbidden construct in src.
// Exceptions match path relative to this package, so a subpackage file named
// transport.go gets none; allowExceptions is false for ghsnapshot files.
// appPath names the functions of src exempt from some rules, by rule.
func scanForBypass(t *testing.T, path, src string, allowExceptions bool, appPath map[string][]string) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	rel := filepath.Clean(path)
	var findings []string
	for _, decl := range file.Decls {
		var exempt []string
		if fn, ok := decl.(*ast.FuncDecl); ok {
			exempt = appPath[fn.Name.Name]
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			if n == nil {
				return false
			}
			for _, rule := range bypassRules {
				if !rule.match(n) || slices.Contains(exempt, rule.what) {
					continue
				}
				if allowExceptions && rule.allowedIn == rel {
					continue
				}
				findings = append(findings, fset.Position(n.Pos()).String()+": "+rule.what)
			}
			return true
		})
	}
	return findings
}

// goSources lists the non-test Go files under dir, including subpackages.
func goSources(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func readSource(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestNothingBypassesRepoClient keeps every GitHub request and every token
// read in transport.go and config.go, so RepoClient stays the only way to a
// repository. It covers this package with its subpackages and every
// ghsnapshot file the fork changed.
func TestNothingBypassesRepoClient(t *testing.T) {
	var findings []string
	own := goSources(t, ".")
	if len(own) == 0 {
		t.Fatal("no githubissues sources found")
	}
	for _, path := range own {
		findings = append(findings, scanForBypass(t, path, readSource(t, path), true, nil)...)
	}

	var forkChanged []string
	for _, path := range goSources(t, filepath.Join("..", "integrations", "ghsnapshot")) {
		src := readSource(t, path)
		if strings.Contains(src, forkMarker) {
			forkChanged = append(forkChanged, filepath.Base(path))
			findings = append(findings, scanForBypass(t, path, src, false, upstreamAppPath[filepath.Base(path)])...)
		}
	}
	if len(forkChanged) == 0 {
		t.Fatal("found no fork-changed ghsnapshot file; the marker or the path moved")
	}
	t.Logf("scanned githubissues files %v and fork-changed ghsnapshot files %v", own, forkChanged)

	for _, f := range findings {
		t.Errorf("bypasses RepoClient: %s", f)
	}
}

// TestBypassScanCatchesEachRule proves the scan is not vacuous: each
// forbidden construct in a file without an exception is reported.
func TestBypassScanCatchesEachRule(t *testing.T) {
	src := `package x

import (
	"fmt"
	"net/http"
	"os"
)

type svc struct {
	token      string
	httpClient *http.Client
	cfg        struct{ token string }
}

func (s *svc) leak() {
	req, _ := http.NewRequest("GET", "https://api.github.com/user", nil)
	req.Header.Set("Authorization", "Bearer "+s.cfg.token)
	_, _ = http.DefaultClient.Do(req)
	_, _ = s.httpClient.Do(req)
	_ = os.Getenv("MULTICA_GITHUB_TOKEN")
	_ = lookup(EnvToken)
}

func (s *svc) message() string {
	return fmt.Sprintf("set %s and restart", EnvToken)
}
`
	findings := strings.Join(scanForBypass(t, "poller.go", src, true, nil), "\n")
	if strings.Contains(findings, "poller.go:25:") {
		t.Errorf("naming the variable in a message was reported:\n%s", findings)
	}
	for _, rule := range bypassRules {
		if !strings.Contains(findings, ": "+rule.what) {
			t.Errorf("rule %q not reported; findings:\n%s", rule.what, findings)
		}
	}

	// The exceptions hold only in their own file.
	if got := scanForBypass(t, transportFile, `package x
import "net/http"
func f(s *svc) { r, _ := http.NewRequest("GET", "u", nil); r.Header.Set("Authorization", s.cfg.token) }
`, true, nil); len(got) != 0 {
		t.Errorf("transport.go exceptions not honored: %v", got)
	}
	if got := scanForBypass(t, transportFile, `package x
import "net/http"
func f() { _, _ = http.NewRequest("GET", "u", nil) }
`, false, nil); len(got) == 0 {
		t.Error("a ghsnapshot file named transport.go must not inherit the exception")
	}
	// The App path exemptions hold only in the named function, for the named
	// rules.
	appSrc := `package x
import "net/http"
func restGet(s *svc) { r, _ := http.NewRequest("GET", "u", nil); r.Header.Set("Authorization", s.cfg.token) }
func staticGet() { _, _ = http.NewRequest("GET", "u", nil) }
`
	var inRestGet, inStaticGet []string
	for _, f := range scanForBypass(t, "client.go", appSrc, false, upstreamAppPath["client.go"]) {
		switch {
		case strings.HasPrefix(f, "client.go:3:"):
			inRestGet = append(inRestGet, f[strings.LastIndex(f, ": ")+2:])
		case strings.HasPrefix(f, "client.go:4:"):
			inStaticGet = append(inStaticGet, f[strings.LastIndex(f, ": ")+2:])
		}
	}
	if !slices.Equal(inRestGet, []string{"token field read"}) {
		t.Errorf("restGet findings = %v, want only the token field read", inRestGet)
	}
	if !slices.Equal(inStaticGet, []string{"http.NewRequest"}) {
		t.Errorf("a function outside the App path must be scanned in full; findings = %v", inStaticGet)
	}
	if got := scanForBypass(t, filepath.Join("issuekey", transportFile), `package x
import "net/http"
func f() { _, _ = http.NewRequest("GET", "u", nil) }
`, true, nil); len(got) == 0 {
		t.Error("a subpackage file named transport.go must not inherit the exception")
	}
}
