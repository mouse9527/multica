// Modified for the mouse9527/multica fork: Router MVP, slices S1 (M-85) and S2 (M-86), Router v6 slice 2 (M-97), Router inbox S3b (M-133), the Super Agent name in help (M-148), and the creator subscription in subscriber help (M-175).

package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestParseRouterSince(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	cases := map[string]string{
		"":                          "",
		"2h":                        "2026-11-13T10:00:00Z",
		"2026-11-13T08:00:00+08:00": "2026-11-13T00:00:00Z",
	}
	for in, want := range cases {
		got, err := parseRouterSince(in, now)
		if err != nil || got != want {
			t.Errorf("parseRouterSince(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"yesterday", "-1h", "0s"} {
		if _, err := parseRouterSince(bad, now); err == nil {
			t.Errorf("parseRouterSince(%q) accepted", bad)
		}
	}
}

func TestRouterCommandsAreRegistered(t *testing.T) {
	for _, path := range [][]string{
		{"router", "subscribe"}, {"router", "list"}, {"router", "get"}, {"router", "pause"},
		{"router", "resume"}, {"router", "delete"}, {"router", "events"}, {"router", "scope"},
		{"router", "update"}, {"router", "state", "get"}, {"router", "state", "set"},
		{"router", "runs"}, {"router", "changes"}, {"router", "release"}, {"router", "holds"}, {"notify", "owner"}, {"notify", "list"},
	} {
		cmd, _, err := rootCmd.Find(path)
		if err != nil || cmd.Name() != path[len(path)-1] {
			t.Errorf("multica %v: %v", path, err)
		}
	}
	for _, flag := range []string{"anchor", "debounce", "max-wait", "max-runs-per-hour"} {
		if routerSubscribeCmd.Flags().Lookup(flag) == nil || routerUpdateCmd.Flags().Lookup(flag) == nil {
			t.Errorf("--%s missing on subscribe or update", flag)
		}
	}
}

func TestRouterInboxRulesOutput(t *testing.T) {
	if got := holdVerdict(map[string]any{"verdict": "handle", "rule": "comment"}); got != "handle (comment)" {
		t.Fatalf("holdVerdict = %q", got)
	}
	if got := holdVerdict(map[string]any{"verdict": ""}); got != "" {
		t.Fatalf("unclassified holdVerdict = %q", got)
	}
	rows := ruleReportRows([]any{
		map[string]any{"verdict": "handle", "rule": "comment", "samples": float64(4), "acted": float64(1), "rate": 0.25, "pending": float64(2)},
		map[string]any{"verdict": "gray", "rule": "mention", "samples": float64(0), "acted": float64(0), "rate": nil, "pending": float64(1)},
	}, false)
	if len(rows) != 2 || rows[0][4] != "25.0%" || rows[0][2] != "4" || rows[1][4] != "-" || rows[1][5] != "1" {
		t.Fatalf("rows = %v", rows)
	}
	if totals := ruleReportRows([]any{map[string]any{"verdict": "handle", "rule": ""}}, true); totals[0][1] != "(all)" {
		t.Fatalf("totals = %v", totals)
	}
}

// renderHelp returns what `multica <path...> --help` prints.
func renderHelp(t *testing.T, path ...string) string {
	t.Helper()
	cmd := rootCmd
	if len(path) > 0 {
		var err error
		if cmd, _, err = rootCmd.Find(path); err != nil {
			t.Fatalf("multica %v: %v", path, err)
		}
	}
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	t.Cleanup(func() { rootCmd.SetOut(nil) })
	if err := cmd.Help(); err != nil {
		t.Fatalf("multica %v --help: %v", path, err)
	}
	return out.String()
}

// M-148: an agent asked to "make this chat a Super Agent" searched the CLI help
// for that phrase, found nothing (router was not even listed), and fell back to
// `issue subscriber add`. The phrase and the way to router must be reachable
// from the help an agent actually reads.
func TestHelpLeadsFromSuperAgentToRouter(t *testing.T) {
	root := renderHelp(t)
	var routerLine string
	for _, line := range strings.Split(root, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "router:") {
			routerLine = line
		}
	}
	if !strings.Contains(routerLine, "Super Agent") {
		t.Errorf("`multica --help` must list router as the Super Agent command, got line %q in:\n%s", routerLine, root)
	}

	if got := renderHelp(t, "router"); !strings.Contains(got, "router subscribe --issue M-12 --anchor-chat current") ||
		!strings.Contains(got, "not an issue subscription") {
		t.Errorf("`multica router --help` must show how to make a chat a Super Agent and how it differs from issue subscribers:\n%s", got)
	}

	for _, path := range [][]string{{"issue"}, {"issue", "subscriber", "add"}} {
		if got := renderHelp(t, path...); !strings.Contains(got, "multica router subscribe") {
			t.Errorf("`multica %s --help` must point a Super Agent subscription at router:\n%s", strings.Join(path, " "), got)
		}
	}
}

// M-175: a Super Agent that created an issue found itself among the issue's
// subscribers (reason `creator`) and told the owner it had subscribed. The
// subscriber help says that is not a Super Agent scope and how to check one.
func TestIssueSubscriberHelpExplainsTheCreatorSubscription(t *testing.T) {
	for _, path := range [][]string{{"issue", "subscriber", "list"}, {"issue", "subscriber", "add"}} {
		got := renderHelp(t, path...)
		for _, want := range []string{"reason `creator`", "multica router scope <subscription-id>"} {
			if !strings.Contains(got, want) {
				t.Errorf("`multica %s --help` lacks %q:\n%s", strings.Join(path, " "), want, got)
			}
		}
	}
}

// The help rename must not touch what scripts and skills already call.
func TestRouterKeepsItsTechnicalName(t *testing.T) {
	cmd, _, err := rootCmd.Find([]string{"router", "subscribe"})
	if err != nil || cmd.Parent().Name() != "router" {
		t.Fatalf("multica router subscribe: %v", err)
	}
	for _, flag := range []string{"agent", "issue", "project", "events", "anchor-chat", "output"} {
		if cmd.Flags().Lookup(flag) == nil {
			t.Errorf("multica router subscribe lost --%s", flag)
		}
	}
	for _, c := range []*cobra.Command{routerGetCmd, routerPauseCmd, routerResumeCmd, routerDeleteCmd, routerUpdateCmd, routerReleaseCmd} {
		if c.Flags().Lookup("chat") == nil {
			t.Errorf("multica router %s lost --chat", c.Name())
		}
	}
}
