// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

package mcpoauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/pkg/remotemcp"
)

// fakeOAuthServer is an MCP server that is its own authorization server, shaped
// like a gateway with gateway-level dynamic registration: protected resource
// metadata at the path-inserted well-known URL, an issuer with a path, a
// public client, S256 only, rotating refresh tokens.
type fakeOAuthServer struct {
	srv *httptest.Server

	mu            sync.Mutex
	redirectURIs  []string
	codes         map[string]url.Values
	tokenRequests []url.Values
	// refreshStatus, when non-zero, is the status the refresh grant answers.
	refreshStatus int
}

const fakeAccessLifetime = 3600

func newFakeOAuthServer(t *testing.T) *fakeOAuthServer {
	t.Helper()
	f := &fakeOAuthServer{codes: map[string]url.Values{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /mcp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+baseURL(r)+`/.well-known/oauth-protected-resource/mcp"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{
			"resource":              baseURL(r) + "/mcp",
			"authorization_servers": []string{baseURL(r) + "/mcp"},
			"scopes_supported":      []string{},
		})
	})
	mux.HandleFunc("GET /.well-known/oauth-authorization-server/mcp", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{
			"issuer":                                baseURL(r) + "/mcp",
			"authorization_endpoint":                baseURL(r) + "/authorize",
			"token_endpoint":                        baseURL(r) + "/token",
			"registration_endpoint":                 baseURL(r) + "/register",
			"code_challenge_methods_supported":      []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{"none"},
		})
	})
	mux.HandleFunc("POST /register", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			RedirectURIs []string `json:"redirect_uris"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.redirectURIs = append(f.redirectURIs, body.RedirectURIs...)
		f.mu.Unlock()
		writeTestJSON(w, map[string]any{"client_id": "client-xyz", "token_endpoint_auth_method": "none"})
	})
	mux.HandleFunc("POST /token", f.token)
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)

	// pkg/remotemcp reaches only public HTTPS; its operator dev-origin opt-in
	// is the one supported way to point it at a local server, with this
	// server's certificate as the trusted CA.
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw})
	if err := os.WriteFile(caPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(remotemcp.DevOriginsEnv, f.srv.URL)
	t.Setenv(remotemcp.DevCAEnv, caPath)
	return f
}

// baseURL is the server's own origin as the request reached it. Handlers use
// it rather than f.srv, which is assigned after the server starts.
func baseURL(r *http.Request) string { return "https://" + r.Host }

func writeTestJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// authorize plays the user approving in the browser: it checks the request
// the authorization URL makes and issues a code bound to its PKCE challenge.
func (f *fakeOAuthServer) authorize(t *testing.T, authorizationURL string) (code, state string) {
	t.Helper()
	u, err := url.Parse(authorizationURL)
	if err != nil || !strings.HasPrefix(authorizationURL, f.srv.URL+"/authorize?") {
		t.Fatalf("authorization URL %q", authorizationURL)
	}
	q := u.Query()
	for key, want := range map[string]string{
		"response_type": "code", "client_id": "client-xyz", "code_challenge_method": "S256", "resource": f.srv.URL + "/mcp",
	} {
		if got := q.Get(key); got != want {
			t.Fatalf("authorization URL %s = %q, want %q", key, got, want)
		}
	}
	if q.Get("state") == "" || q.Get("code_challenge") == "" || q.Get("redirect_uri") == "" {
		t.Fatalf("authorization URL lacks state, challenge or redirect: %s", authorizationURL)
	}
	code = "code-" + q.Get("state")[:8]
	f.mu.Lock()
	f.codes[code] = q
	f.mu.Unlock()
	return code, q.Get("state")
}

func (f *fakeOAuthServer) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenRequests = append(f.tokenRequests, r.PostForm)
	if r.PostForm.Get("resource") != baseURL(r)+"/mcp" || r.PostForm.Get("client_id") != "client-xyz" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		issued, ok := f.codes[r.PostForm.Get("code")]
		delete(f.codes, r.PostForm.Get("code"))
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != issued.Get("code_challenge") ||
			r.PostForm.Get("redirect_uri") != issued.Get("redirect_uri") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		writeTestJSON(w, map[string]any{"access_token": "at-1", "token_type": "Bearer", "expires_in": fakeAccessLifetime, "refresh_token": "rt-1"})
	case "refresh_token":
		if f.refreshStatus != 0 {
			w.WriteHeader(f.refreshStatus)
			return
		}
		writeTestJSON(w, map[string]any{"access_token": "at-2", "token_type": "bearer", "expires_in": fakeAccessLifetime, "refresh_token": "rt-2"})
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func (f *fakeOAuthServer) setRefreshStatus(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshStatus = status
}

func (f *fakeOAuthServer) registered() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.redirectURIs...)
}

func (f *fakeOAuthServer) tokenRequestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tokenRequests)
}

const testRedirectURI = "https://app.example.test" + CallbackPath

func newFlowHarness(t *testing.T) (*fakeOAuthServer, *Store, fixture, Server) {
	t.Helper()
	fake := newFakeOAuthServer(t)
	store := newTestStore(t, testPool(t), newClock())
	f := newFixture()
	server, err := store.CreateServer(context.Background(), ServerInput{
		WorkspaceID: f.workspaceID, AgentID: f.agentID, CreatedBy: f.ownerID, Name: "gateway", URL: fake.srv.URL + "/mcp",
	})
	if err != nil {
		t.Fatal(err)
	}
	return fake, store, f, server
}

// End to end against the fake server: discovery, registration, PKCE,
// exchange, storage, then refresh through the production Refresher.
func TestConnectFlowEndToEnd(t *testing.T) {
	ctx := context.Background()
	fake, store, f, server := newFlowHarness(t)
	flow := Flow{Store: store}

	authorizationURL, err := flow.Start(ctx, server, f.ownerID, testRedirectURI, "/ws/agents/x")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if got := fake.registered(); len(got) != 1 || got[0] != testRedirectURI {
		t.Fatalf("registered redirect URIs %v", got)
	}
	code, state := fake.authorize(t, authorizationURL)

	pending, err := flow.Complete(ctx, state, code, f.ownerID, func(p PendingAuthorization) error {
		if p.ServerID != server.ID || p.ReturnTo != "/ws/agents/x" {
			t.Errorf("pending = %+v", p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if pending.ServerID != server.ID {
		t.Fatalf("completed %+v", pending)
	}
	status, err := store.GetConnectionStatus(ctx, f.workspaceID, server.ID, f.ownerID)
	if err != nil || status.Status != StatusConnected || status.ExpiresAt == nil {
		t.Fatalf("status = %+v, err = %v", status, err)
	}

	// The state worked once.
	if _, err := flow.Complete(ctx, state, code, f.ownerID, nil); !errors.Is(err, ErrStateInvalid) {
		t.Fatalf("replayed state: err = %v", err)
	}

	// Refresh through the production Refresher: a rejected grant is
	// ErrGrantRejected, a 5xx is not, success rotates.
	source := NewTokenSource(store, Refresh, nil)
	request := RefreshRequest{
		TokenEndpoint: fake.srv.URL + "/token", Resource: fake.srv.URL + "/mcp", RefreshToken: NewSecret("rt-1"),
		Client: remotemcp.OAuthClientRegistration{ClientID: "client-xyz", TokenEndpointAuthMethod: "none"},
	}
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized} {
		fake.setRefreshStatus(status)
		if _, err := Refresh(ctx, request); !errors.Is(err, ErrGrantRejected) {
			t.Errorf("refresh answered %d: err = %v, want ErrGrantRejected", status, err)
		}
	}
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusTooManyRequests, http.StatusInternalServerError} {
		fake.setRefreshStatus(status)
		if _, err := Refresh(ctx, request); err == nil || errors.Is(err, ErrGrantRejected) {
			t.Errorf("refresh answered %d: err = %v, want a transient error", status, err)
		}
	}
	fake.setRefreshStatus(0)
	response, err := Refresh(ctx, request)
	if err != nil || response.AccessToken != "at-2" || response.RefreshToken != "rt-2" {
		t.Fatalf("refresh = %+v, err = %v", response, err)
	}
	// The stored token from the exchange is fresh, so the source hands it out.
	credential, err := source.Credential(ctx, server.ID, f.ownerID)
	if err != nil || credential.AccessToken.Reveal() != "at-1" {
		t.Fatalf("credential = %v, err = %v", credential.AccessToken.Reveal(), err)
	}
}

// Completing as someone else, or refusing in check, never exchanges the code.
func TestCompleteRefusesBeforeExchanging(t *testing.T) {
	ctx := context.Background()
	fake, store, f, server := newFlowHarness(t)
	flow := Flow{Store: store}

	authorizationURL, err := flow.Start(ctx, server, f.ownerID, testRedirectURI, "")
	if err != nil {
		t.Fatal(err)
	}
	code, state := fake.authorize(t, authorizationURL)
	if _, err := flow.Complete(ctx, state, code, newFixture().ownerID, nil); !errors.Is(err, ErrStateInvalid) {
		t.Fatalf("another user: err = %v", err)
	}

	authorizationURL, err = flow.Start(ctx, server, f.ownerID, testRedirectURI, "")
	if err != nil {
		t.Fatal(err)
	}
	code, state = fake.authorize(t, authorizationURL)
	refused := errors.New("no longer the owner")
	if _, err := flow.Complete(ctx, state, code, f.ownerID, func(PendingAuthorization) error { return refused }); !errors.Is(err, refused) {
		t.Fatalf("refused check: err = %v", err)
	}

	authorizationURL, err = flow.Start(ctx, server, f.ownerID, testRedirectURI, "")
	if err != nil {
		t.Fatal(err)
	}
	_, state = fake.authorize(t, authorizationURL)
	if _, err := flow.Complete(ctx, state, "", f.ownerID, nil); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatalf("no code: err = %v", err)
	}

	if n := fake.tokenRequestCount(); n != 0 {
		t.Fatalf("%d token requests, want none", n)
	}
	if _, err := store.GetConnectionStatus(ctx, f.workspaceID, server.ID, f.ownerID); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("a refused completion connected: %v", err)
	}
}

// Without the operator opt-in pkg/remotemcp refuses a non-public address: the
// SSRF boundary holds for every server an agent owner can name.
func TestStartRefusesNonPublicServers(t *testing.T) {
	_, store, f, server := newFlowHarness(t)
	t.Setenv(remotemcp.DevOriginsEnv, "")
	if _, err := (Flow{Store: store}).Start(context.Background(), server, f.ownerID, testRedirectURI, ""); !errors.Is(err, ErrDiscovery) {
		t.Fatalf("err = %v, want ErrDiscovery", err)
	}
}

func TestRedirectURI(t *testing.T) {
	for appURL, want := range map[string]string{
		"https://multica.example.com":  "https://multica.example.com/oauth/mcp",
		"https://multica.example.com/": "https://multica.example.com/oauth/mcp",
		"http://localhost:3000":        "http://localhost:3000/oauth/mcp",
	} {
		if got, err := RedirectURI(appURL); err != nil || got != want {
			t.Errorf("RedirectURI(%q) = %q, %v; want %q", appURL, got, err, want)
		}
	}
	for _, appURL := range []string{"", "http://multica.example.com", "https://multica.example.com/app", "https://u@multica.example.com", "multica.example.com"} {
		if got, err := RedirectURI(appURL); err == nil {
			t.Errorf("RedirectURI(%q) = %q, want an error", appURL, got)
		}
	}
}
