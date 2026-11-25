// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// upstreamMCP is a fake OAuth-protected MCP server over HTTPS (the proxy
// serves no other target) that records what reached it and accepts only the
// token in want.
type upstreamMCP struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []*http.Request
	body []string
	want string
}

func newUpstreamMCP(t *testing.T, want string, handler http.HandlerFunc) *upstreamMCP {
	t.Helper()
	u := &upstreamMCP{want: want}
	u.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.seen = append(u.seen, r.Clone(context.Background()))
		u.body = append(u.body, string(raw))
		wantAuth := "Bearer " + u.want
		u.mu.Unlock()
		if r.Header.Get("Authorization") != wantAuth {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if handler != nil {
			handler(w, r)
			return
		}
		w.Header().Set("Mcp-Session-Id", "session-1")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *upstreamMCP) requests() ([]*http.Request, []string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]*http.Request(nil), u.seen...), append([]string(nil), u.body...)
}

// countingFetcher hands out the tokens in order, each valid for ttl.
type countingFetcher struct {
	mu       sync.Mutex
	calls    atomic.Int32
	tokens   []string
	ttl      time.Duration
	err      error
	rejected []string
}

func (f *countingFetcher) fetch(_ context.Context, serverID, rejectedHash string) (mcpOAuthCredential, error) {
	n := int(f.calls.Add(1))
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rejected = append(f.rejected, rejectedHash)
	if f.err != nil {
		return mcpOAuthCredential{}, f.err
	}
	token := f.tokens[len(f.tokens)-1]
	if n <= len(f.tokens) {
		token = f.tokens[n-1]
	}
	return mcpOAuthCredential{
		Headers:   map[string]string{"Authorization": "Bearer " + token, "x-mcp-servers": "browser"},
		ExpiresAt: time.Now().Add(f.ttl),
	}, nil
}

func startTestProxy(t *testing.T, upstream *upstreamMCP, fetch mcpOAuthCredentialFetcher) (string, *mcpOAuthProxySet) {
	t.Helper()
	config, set, notices := applyMCPOAuthServers(mcpOAuthMountInput{
		Agent: &AgentData{
			MCPOAuthNames: []string{"gateway"},
			MCPOAuthProxy: []MCPOAuthProxyServerData{{Name: "gateway", ServerID: "srv-1", URL: upstream.srv.URL + "/mcp"}},
		},
		Provider:  "claude",
		Effective: json.RawMessage(`{"mcpServers":{}}`),
		Fetch:     fetch,
		Transport: upstream.srv.Client().Transport,
		Logger:    quietTaskLog(),
	})
	if len(notices) != 0 || set == nil {
		t.Fatalf("notices = %v, set = %v", notices, set)
	}
	t.Cleanup(set.Close)
	var doc struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(config, &doc); err != nil {
		t.Fatal(err)
	}
	entry := doc.MCPServers["gateway"]
	if entry.Type != "http" || !strings.HasPrefix(entry.URL, "http://127.0.0.1:") || len(entry.Headers) != 0 {
		t.Fatalf("gateway entry = %+v, want a loopback URL and no headers", entry)
	}
	return entry.URL, set
}

func postMCP(t *testing.T, url, body string, header http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"?probe=1", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// The proxy forwards the call with the fetched token and the server's extra
// header, drops the runtime's own credentials, keeps the MCP session header
// both ways, and reuses the token until it nears expiry.
func TestMCPOAuthProxyForwardsWithTheFetchedToken(t *testing.T) {
	upstream := newUpstreamMCP(t, "tok-1", nil)
	fetcher := &countingFetcher{tokens: []string{"tok-1"}, ttl: time.Hour}
	proxyURL, _ := startTestProxy(t, upstream, fetcher.fetch)

	header := http.Header{
		"Authorization":  {"Bearer runtime-own-token"},
		"Cookie":         {"session=runtime"},
		"Mcp-Session-Id": {"session-1"},
		"Content-Type":   {"application/json"},
	}
	for i := 0; i < 2; i++ {
		resp := postMCP(t, proxyURL, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, header)
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Mcp-Session-Id") != "session-1" || !strings.Contains(string(raw), `"result"`) {
			t.Fatalf("call %d: %d %v %s", i, resp.StatusCode, resp.Header, raw)
		}
	}
	seen, bodies := upstream.requests()
	if len(seen) != 2 {
		t.Fatalf("upstream saw %d requests, want 2", len(seen))
	}
	for i, r := range seen {
		if r.Header.Get("Authorization") != "Bearer tok-1" || r.Header.Get("x-mcp-servers") != "browser" ||
			r.Header.Get("Cookie") != "" || r.Header.Get("Mcp-Session-Id") != "session-1" ||
			r.URL.Path != "/mcp" || r.URL.Query().Get("probe") != "1" || !strings.Contains(bodies[i], "tools/list") {
			t.Fatalf("upstream request %d: %s %v body %q", i, r.URL, r.Header, bodies[i])
		}
	}
	if n := fetcher.calls.Load(); n != 1 {
		t.Fatalf("%d credential fetches, want 1 (cached)", n)
	}
}

// A token about to expire is fetched again before use: a long task keeps
// working past the first token's lifetime.
func TestMCPOAuthProxyRefetchesAnExpiringToken(t *testing.T) {
	upstream := newUpstreamMCP(t, "tok-1", nil)
	fetcher := &countingFetcher{tokens: []string{"tok-1"}, ttl: mcpOAuthCredentialSlack / 2}
	proxyURL, _ := startTestProxy(t, upstream, fetcher.fetch)
	for i := 0; i < 3; i++ {
		if resp := postMCP(t, proxyURL, `{}`, nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("call %d: %d", i, resp.StatusCode)
		}
	}
	if n := fetcher.calls.Load(); n != 3 {
		t.Fatalf("%d credential fetches, want one per call for a token inside the slack", n)
	}
}

// A 401 (the token was rotated or revoked since it was cached) fetches a
// fresh token and resends the same request once.
func TestMCPOAuthProxyRetriesOnceAfter401(t *testing.T) {
	upstream := newUpstreamMCP(t, "tok-2", nil)
	fetcher := &countingFetcher{tokens: []string{"tok-1", "tok-2"}, ttl: time.Hour}
	proxyURL, _ := startTestProxy(t, upstream, fetcher.fetch)

	resp := postMCP(t, proxyURL, `{"id":7}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200 after the retry", resp.StatusCode)
	}
	seen, bodies := upstream.requests()
	if len(seen) != 2 || seen[0].Header.Get("Authorization") != "Bearer tok-1" || seen[1].Header.Get("Authorization") != "Bearer tok-2" ||
		bodies[0] != `{"id":7}` || bodies[1] != `{"id":7}` {
		t.Fatalf("upstream saw %d requests: %v", len(seen), bodies)
	}
	// The second fetch told the server which token was refused, so the
	// server refreshes it rather than serving it again.
	fetcher.mu.Lock()
	rejected := append([]string(nil), fetcher.rejected...)
	fetcher.mu.Unlock()
	if len(rejected) != 2 || rejected[0] != "" || rejected[1] != bearerTokenHash(map[string]string{"Authorization": "Bearer tok-1"}) {
		t.Fatalf("rejected hashes sent = %q", rejected)
	}

	// Still unauthorized with the fresh token: the 401 goes back, no loop.
	upstream.mu.Lock()
	upstream.want = "tok-never"
	upstream.mu.Unlock()
	if resp := postMCP(t, proxyURL, `{}`, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", resp.StatusCode)
	}
	if seen, _ := upstream.requests(); len(seen) != 4 {
		t.Fatalf("upstream saw %d requests, want 4 (one retry each)", len(seen))
	}
}

func TestMCPOAuthProxyWithoutACredentialCallsNothing(t *testing.T) {
	upstream := newUpstreamMCP(t, "tok-1", nil)
	fetcher := &countingFetcher{err: errors.New("GET credential returned 409: needs reconnect")}
	proxyURL, _ := startTestProxy(t, upstream, fetcher.fetch)
	if resp := postMCP(t, proxyURL, `{}`, nil); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", resp.StatusCode)
	}
	if seen, _ := upstream.requests(); len(seen) != 0 {
		t.Fatalf("upstream saw %d requests without a credential", len(seen))
	}
}

// The entry was pointed at another URL and reconnected during the run: the
// server's credential now names that URL. The proxy, still targeting the old
// one, refuses it before anything is sent, so the new token never reaches
// the old endpoint. A credential naming the proxy's own target is used.
func TestMCPOAuthProxyRefusesACredentialForAnotherURL(t *testing.T) {
	upstream := newUpstreamMCP(t, "tok-new", nil)
	var credentialURL atomic.Value
	credentialURL.Store("https://moved.example.com/mcp")
	fetch := func(context.Context, string, string) (mcpOAuthCredential, error) {
		return mcpOAuthCredential{
			Headers:   map[string]string{"Authorization": "Bearer tok-new"},
			ExpiresAt: time.Now().Add(time.Hour),
			URL:       credentialURL.Load().(string),
		}, nil
	}
	proxyURL, _ := startTestProxy(t, upstream, fetch)
	if resp := postMCP(t, proxyURL, `{}`, nil); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", resp.StatusCode)
	}
	if seen, _ := upstream.requests(); len(seen) != 0 {
		t.Fatalf("upstream saw %d requests with a token for another URL", len(seen))
	}

	credentialURL.Store(upstream.srv.URL + "/mcp")
	if resp := postMCP(t, proxyURL, `{}`, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("own URL: status %d, want 200", resp.StatusCode)
	}
}

// Only the random path is served: another local process that finds the port
// cannot use the token behind it.
func TestMCPOAuthProxyServesOnlyItsPath(t *testing.T) {
	upstream := newUpstreamMCP(t, "tok-1", nil)
	fetcher := &countingFetcher{tokens: []string{"tok-1"}, ttl: time.Hour}
	proxyURL, _ := startTestProxy(t, upstream, fetcher.fetch)
	base := proxyURL[:strings.LastIndex(proxyURL, "/")]
	if resp := postMCP(t, base+"/guess", `{}`, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want 404", resp.StatusCode)
	}
	if n := fetcher.calls.Load(); n != 0 {
		t.Fatalf("a wrong path fetched a credential")
	}
}

// A server-sent event stream reaches the runtime as it arrives, not when the
// MCP server finishes.
func TestMCPOAuthProxyStreamsEvents(t *testing.T) {
	release := make(chan struct{})
	upstream := newUpstreamMCP(t, "tok-1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "data: second\n\n")
	})
	fetcher := &countingFetcher{tokens: []string{"tok-1"}, ttl: time.Hour}
	proxyURL, _ := startTestProxy(t, upstream, fetcher.fetch)

	resp := postMCP(t, proxyURL, `{}`, nil)
	reader := bufio.NewReader(resp.Body)
	got := make(chan string, 1)
	go func() {
		line, _ := reader.ReadString('\n')
		got <- line
	}()
	select {
	case line := <-got:
		if line != "data: first\n" {
			t.Fatalf("first line %q", line)
		}
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("the first event did not arrive before the stream ended")
	}
	close(release)
}

func TestApplyMCPOAuthServersWithholdsNames(t *testing.T) {
	fetcher := &countingFetcher{tokens: []string{"tok-1"}, ttl: time.Hour}
	agent := &AgentData{
		MCPOAuthNames: []string{"gateway", "dead"},
		MCPOAuthProxy: []MCPOAuthProxyServerData{{Name: "gateway", ServerID: "srv-1", URL: "https://mcp.example.com/mcp"}},
	}
	decode := func(t *testing.T, raw json.RawMessage) map[string]map[string]any {
		t.Helper()
		var doc struct {
			MCPServers map[string]map[string]any `json:"mcpServers"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		return doc.MCPServers
	}

	t.Run("runtime's own same-named entries are withheld", func(t *testing.T) {
		config, set, notices := applyMCPOAuthServers(mcpOAuthMountInput{
			Agent: agent, Provider: "claude",
			Effective: json.RawMessage(`{"mcpServers":{"gateway":{"url":"https://static.example","headers":{"Authorization":"Bearer static-shared-key"}},"dead":{"url":"https://dead.example"},"own":{"url":"https://own.example"}}}`),
			Fetch:     fetcher.fetch, Logger: quietTaskLog(),
		})
		defer set.Close()
		servers := decode(t, config)
		if len(notices) != 0 || len(servers) != 2 || servers["own"] == nil || servers["dead"] != nil {
			t.Fatalf("servers = %v, notices = %v", servers, notices)
		}
		if strings.Contains(string(config), "static-shared-key") || !strings.HasPrefix(servers["gateway"]["url"].(string), "http://127.0.0.1:") {
			t.Fatalf("config = %s", config)
		}
	})

	t.Run("an unmanaged run becomes managed without the names", func(t *testing.T) {
		config, set, _ := applyMCPOAuthServers(mcpOAuthMountInput{
			Agent: agent, Provider: "claude", Effective: nil,
			RuntimeConfig: func(json.RawMessage) (json.RawMessage, error) {
				return json.RawMessage(`{"mcpServers":{"gateway":{"url":"https://local-static.example"},"local":{"command":"x"}}}`), nil
			},
			Fetch: fetcher.fetch, Logger: quietTaskLog(),
		})
		defer set.Close()
		servers := decode(t, config)
		if len(servers) != 2 || servers["local"] == nil || strings.Contains(string(config), "local-static") {
			t.Fatalf("config = %s", config)
		}
	})

	t.Run("a runtime without the proxy does not get the server", func(t *testing.T) {
		before := fetcher.calls.Load()
		config, set, notices := applyMCPOAuthServers(mcpOAuthMountInput{
			Agent: agent, Provider: "cursor", Effective: json.RawMessage(`{"mcpServers":{"gateway":{"url":"https://static.example","headers":{"Authorization":"Bearer static-shared-key"}},"own":{"url":"https://own.example"}}}`),
			Fetch: fetcher.fetch, Logger: quietTaskLog(),
		})
		servers := decode(t, config)
		if set != nil || len(servers) != 1 || servers["own"] == nil {
			t.Fatalf("set = %v, servers = %v", set, servers)
		}
		if strings.Contains(string(config), "Bearer") {
			t.Fatalf("a token reached a runtime without the proxy: %s", config)
		}
		if len(notices) != 1 || !strings.Contains(notices[0], `"gateway"`) || !strings.Contains(notices[0], "cursor") {
			t.Fatalf("notices = %v", notices)
		}
		if fetcher.calls.Load() != before {
			t.Fatal("a token was fetched for a runtime that cannot use the proxy")
		}
	})

	t.Run("an unreadable run config: nothing is mounted, and the transcript says so", func(t *testing.T) {
		config, set, notices := applyMCPOAuthServers(mcpOAuthMountInput{
			Agent: agent, Provider: "claude", Effective: json.RawMessage(`{"mcpServers":`),
			Fetch: fetcher.fetch, Logger: quietTaskLog(),
		})
		if set != nil || string(config) != `{"mcpServers":{}}` {
			t.Fatalf("config = %s", config)
		}
		if len(notices) != 1 || notices[0] != mcpOAuthConfigUnreadableNotice {
			t.Fatalf("notices = %v", notices)
		}
	})

	t.Run("a target that is not HTTPS is not proxied", func(t *testing.T) {
		before := fetcher.calls.Load()
		config, set, notices := applyMCPOAuthServers(mcpOAuthMountInput{
			Agent: &AgentData{
				MCPOAuthNames: []string{"gateway"},
				MCPOAuthProxy: []MCPOAuthProxyServerData{{Name: "gateway", ServerID: "srv-1", URL: "http://mcp.example.com/mcp"}},
			},
			Provider: "claude", Effective: json.RawMessage(`{"mcpServers":{}}`),
			Fetch: fetcher.fetch, Logger: quietTaskLog(),
		})
		if set != nil || len(decode(t, config)) != 0 {
			t.Fatalf("set = %v, config = %s", set, config)
		}
		if len(notices) != 1 || !strings.Contains(notices[0], `"gateway"`) {
			t.Fatalf("notices = %v", notices)
		}
		if fetcher.calls.Load() != before {
			t.Fatal("a token was fetched for a plain-HTTP target")
		}
	})

	t.Run("names unreadable on the server: nothing is mounted", func(t *testing.T) {
		config, set, _ := applyMCPOAuthServers(mcpOAuthMountInput{
			Agent: &AgentData{MCPOAuthWithholdAll: true}, Provider: "claude",
			Effective: json.RawMessage(`{"mcpServers":{"own":{"url":"https://own.example"}}}`),
			Fetch:     fetcher.fetch, Logger: quietTaskLog(),
		})
		if set != nil || string(config) != `{"mcpServers":{}}` {
			t.Fatalf("config = %s", config)
		}
	})

	t.Run("nothing from the server leaves the config alone", func(t *testing.T) {
		in := json.RawMessage(`{"mcpServers":{"own":{"url":"https://own.example"}}}`)
		config, set, notices := applyMCPOAuthServers(mcpOAuthMountInput{Agent: &AgentData{}, Provider: "claude", Effective: in, Logger: quietTaskLog()})
		if string(config) != string(in) || set != nil || notices != nil {
			t.Fatalf("config = %s", config)
		}
		if config, _, _ := applyMCPOAuthServers(mcpOAuthMountInput{Provider: "claude", Effective: nil, Logger: quietTaskLog()}); config != nil {
			t.Fatalf("no agent: config = %s", config)
		}
	})
}

// Closing the run's proxies while an event stream is open ends it on both
// sides: the upstream call is cancelled and the runtime's connection is
// closed, so nothing keeps reaching the MCP server after the run.
func TestMCPOAuthProxySetCloseEndsOpenStreams(t *testing.T) {
	upstreamEnded := make(chan struct{})
	upstream := newUpstreamMCP(t, "tok-1", func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamEnded)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	fetcher := &countingFetcher{tokens: []string{"tok-1"}, ttl: time.Hour}
	proxyURL, set := startTestProxy(t, upstream, fetcher.fetch)

	resp := postMCP(t, proxyURL, `{}`, nil)
	reader := bufio.NewReader(resp.Body)
	if line, err := reader.ReadString('\n'); err != nil || line != "data: first\n" {
		t.Fatalf("first line %q, %v", line, err)
	}
	clientEnded := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, reader)
		clientEnded <- err
	}()

	closed := make(chan struct{})
	go func() {
		set.Close()
		close(closed)
	}()
	for name, ch := range map[string]<-chan struct{}{"Close": closed, "the upstream call": upstreamEnded} {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s did not end after Close", name)
		}
	}
	select {
	case <-clientEnded:
	case <-time.After(5 * time.Second):
		t.Fatal("the runtime's stream stayed open after Close")
	}
}

// Closing the run's proxies stops serving: nothing keeps the token reachable
// after the task.
func TestMCPOAuthProxySetClose(t *testing.T) {
	upstream := newUpstreamMCP(t, "tok-1", nil)
	fetcher := &countingFetcher{tokens: []string{"tok-1"}, ttl: time.Hour}
	proxyURL, set := startTestProxy(t, upstream, fetcher.fetch)
	set.Close()
	set.Close() // idempotent
	if _, err := http.Post(proxyURL, "application/json", strings.NewReader(`{}`)); err == nil {
		t.Fatal("the proxy still answers after Close")
	}
}
