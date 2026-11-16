// Modified for the mouse9527/multica fork: Router MVP, slices S1 (M-85) and S2 (M-86).

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
		{"router", "runs"}, {"router", "changes"}, {"router", "release"}, {"notify", "owner"}, {"notify", "list"},
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
