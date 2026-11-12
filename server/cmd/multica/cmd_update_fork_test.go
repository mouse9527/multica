// This file belongs to the mouse9527/multica fork (not present upstream).
package main

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestRunUpdateAlreadyLatestOnFork(t *testing.T) {
	var (
		mu       sync.Mutex
		requests []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.URL.Path)
		mu.Unlock()
		if r.URL.Path != "/repos/mouse9527/multica/releases/latest" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name":"v0.8.1-mouse.2","assets":[]}`))
	}))
	defer srv.Close()
	t.Setenv("MULTICA_RELEASE_API_BASE_URL", srv.URL)
	t.Setenv("MULTICA_RELEASE_REPO", "")

	prevVersion := version
	version = "0.8.1-mouse.2"
	t.Cleanup(func() { version = prevVersion })

	if err := runUpdate(nil, nil); err != nil {
		t.Fatalf("runUpdate() error = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 1 || requests[0] != "/repos/mouse9527/multica/releases/latest" {
		t.Fatalf("requests = %v, want only the fork's latest release lookup", requests)
	}
}
