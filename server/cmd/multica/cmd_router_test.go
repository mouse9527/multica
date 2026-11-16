// Modified for the mouse9527/multica fork: Router MVP, slice S1 (M-85).

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
	for _, name := range []string{"subscribe", "list", "get", "pause", "resume", "delete", "events", "scope"} {
		cmd, _, err := rootCmd.Find([]string{"router", name})
		if err != nil || cmd.Name() != name {
			t.Errorf("multica router %s: %v", name, err)
		}
	}
}
