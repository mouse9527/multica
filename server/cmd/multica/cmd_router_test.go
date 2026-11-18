// Modified for the mouse9527/multica fork: Router MVP, slices S1 (M-85) and S2 (M-86), Router v6 slice 2 (M-97), and Router inbox S3b (M-133).

package main

import (
	"testing"
	"time"
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
