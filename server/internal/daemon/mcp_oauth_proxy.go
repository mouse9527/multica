// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/multica-ai/multica/server/pkg/httptransport"
)

// MCPOAuthProxyServerData is an OAuth MCP server the server asked this daemon
// to serve through a local proxy. It carries no token.
type MCPOAuthProxyServerData struct {
	Name     string `json:"name"`
	ServerID string `json:"server_id"`
	URL      string `json:"url"`
}

// mcpOAuthCredential is the current token for one server, as the server's
// credential endpoint returns it.
type mcpOAuthCredential struct {
	Headers   map[string]string
	ExpiresAt time.Time
}

// mcpOAuthCredentialFetcher gets the current credential for a server of the
// running task from the Multica server. rejectedHash, when set, is the hex
// SHA-256 of a token the MCP server just refused, so the server refreshes it
// instead of handing it back.
type mcpOAuthCredentialFetcher func(ctx context.Context, serverID, rejectedHash string) (mcpOAuthCredential, error)

const (
	// mcpOAuthProxyMaxBody bounds one MCP request body, which the proxy
	// buffers so it can resend it once with a fresh token after a 401. It
	// matches the Remote MCP client's response cap (remotemcp.MaxResponseBytes):
	// a JSON-RPC call larger than that is not a tool call this proxy should
	// hold in memory. A larger body gets 413 and the runtime sees the error.
	mcpOAuthProxyMaxBody = 4 << 20
	// mcpOAuthCredentialSlack: a cached token that expires within it is
	// fetched again before use.
	mcpOAuthCredentialSlack = time.Minute
	// mcpOAuthCredentialFetchTimeout bounds one credential fetch. It runs
	// under the proxy's lock on purpose (one fetch at a time, so a 401 burst
	// asks the server once), so it also bounds how long a parallel call can
	// wait behind it. The server answers from its cache unless a refresh is
	// due, and its own refresh is bounded at 10s.
	mcpOAuthCredentialFetchTimeout = 10 * time.Second
)

// hopHeaders are not forwarded in either direction, and the runtime's own
// credentials never reach the MCP server: the proxy supplies them.
var hopHeaders = map[string]bool{
	"Connection": true, "Proxy-Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true,
	"Proxy-Authorization": true, "Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true,
}

var proxyDroppedRequestHeaders = map[string]bool{"Authorization": true, "Cookie": true, "Host": true, "Content-Length": true}

// mcpOAuthProxy serves one OAuth MCP server on a loopback listener. Each
// request is forwarded to the server's URL with the current token: the
// credential is cached until shortly before it expires, and a 401 from the
// MCP server fetches a fresh one and resends the request once. The runtime's
// MCP config therefore holds only http://127.0.0.1:<port>/<random path>; no
// token is written to it, and none outlives the run.
type mcpOAuthProxy struct {
	name     string
	serverID string
	target   *url.URL
	path     string
	fetch    mcpOAuthCredentialFetcher
	client   *http.Client
	logger   *slog.Logger

	mu     sync.Mutex
	cached *mcpOAuthCredential
}

// newMCPOAuthProxy builds the proxy for one server. Only an HTTPS target is
// served: the server stores nothing else, and a token must not travel in the
// clear. transport is nil in production (a fresh clone of the default);
// tests pass one that trusts their TLS server.
func newMCPOAuthProxy(server MCPOAuthProxyServerData, fetch mcpOAuthCredentialFetcher, transport http.RoundTripper, logger *slog.Logger) (*mcpOAuthProxy, error) {
	target, err := url.Parse(server.URL)
	if err != nil || target.Scheme != "https" || target.Host == "" || target.User != nil {
		return nil, fmt.Errorf("MCP OAuth server %q has an unusable URL", server.Name)
	}
	pathToken := make([]byte, 24)
	if _, err := rand.Read(pathToken); err != nil {
		return nil, err
	}
	if transport == nil {
		defaultTransport := httptransport.CloneDefault().(*http.Transport)
		// MCP over HTTP answers with a stream for as long as the call runs,
		// so there is no overall client timeout; only the wait for headers
		// is bounded.
		defaultTransport.ResponseHeaderTimeout = 2 * time.Minute
		transport = defaultTransport
	}
	return &mcpOAuthProxy{
		name: server.Name, serverID: server.ServerID, target: target,
		path:  "/" + hex.EncodeToString(pathToken),
		fetch: fetch, logger: logger,
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("MCP OAuth proxy does not follow redirects")
			},
		},
	}, nil
}

// credential returns the cached credential, or fetches one when there is
// none or it is about to expire. rejected is the credential the MCP server
// just answered 401 to: the server is told which token it was, so it
// refreshes that token instead of returning it again (unless it has already
// been replaced, in which case the current one comes back).
func (p *mcpOAuthProxy) credential(ctx context.Context, rejected *mcpOAuthCredential) (mcpOAuthCredential, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if rejected == nil && p.cached != nil && (p.cached.ExpiresAt.IsZero() || time.Until(p.cached.ExpiresAt) > mcpOAuthCredentialSlack) {
		return *p.cached, nil
	}
	rejectedHash := ""
	if rejected != nil {
		rejectedHash = bearerTokenHash(rejected.Headers)
	}
	fetchCtx, cancel := context.WithTimeout(ctx, mcpOAuthCredentialFetchTimeout)
	defer cancel()
	credential, err := p.fetch(fetchCtx, p.serverID, rejectedHash)
	if err != nil {
		p.cached = nil
		return mcpOAuthCredential{}, err
	}
	p.cached = &credential
	return credential, nil
}

func (p *mcpOAuthProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != p.path {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, mcpOAuthProxyMaxBody+1))
	if err != nil {
		http.Error(w, "could not read the request", http.StatusBadRequest)
		return
	}
	if len(body) > mcpOAuthProxyMaxBody {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}
	response, used, err := p.forward(r, body, nil)
	if err == nil && response.StatusCode == http.StatusUnauthorized {
		// The token may have been revoked or rotated since it was cached:
		// try once more with a replacement.
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, mcpOAuthProxyMaxBody))
		response.Body.Close()
		response, _, err = p.forward(r, body, &used)
	}
	if err != nil {
		// The error text never carries the token: fetch errors come from
		// the daemon client, request errors from net/http.
		p.logger.Warn("MCP OAuth proxy call failed", "server", p.name, "error", err)
		http.Error(w, "MCP server "+p.name+" is unavailable", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	for key, values := range response.Header {
		if hopHeaders[http.CanonicalHeaderKey(key)] {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	streamResponse(w, response.Body)
}

// forward sends the request with the current credential and returns the
// credential it used.
func (p *mcpOAuthProxy) forward(r *http.Request, body []byte, rejected *mcpOAuthCredential) (*http.Response, mcpOAuthCredential, error) {
	credential, err := p.credential(r.Context(), rejected)
	if err != nil {
		return nil, mcpOAuthCredential{}, fmt.Errorf("credential: %w", err)
	}
	target := *p.target
	if r.URL.RawQuery != "" {
		if target.RawQuery != "" {
			target.RawQuery += "&" + r.URL.RawQuery
		} else {
			target.RawQuery = r.URL.RawQuery
		}
	}
	outbound, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, credential, err
	}
	for key, values := range r.Header {
		canonical := http.CanonicalHeaderKey(key)
		if hopHeaders[canonical] || proxyDroppedRequestHeaders[canonical] {
			continue
		}
		for _, value := range values {
			outbound.Header.Add(key, value)
		}
	}
	for key, value := range credential.Headers {
		outbound.Header.Set(key, value)
	}
	response, err := p.client.Do(outbound)
	return response, credential, err
}

// bearerTokenHash is the hex SHA-256 of the bearer token in headers, the
// form the server's credential endpoint takes a rejected token in.
func bearerTokenHash(headers map[string]string) string {
	for key, value := range headers {
		if !strings.EqualFold(key, "Authorization") {
			continue
		}
		if token, ok := strings.CutPrefix(value, "Bearer "); ok {
			sum := sha256.Sum256([]byte(token))
			return hex.EncodeToString(sum[:])
		}
	}
	return ""
}

// streamResponse copies the body and flushes as it goes, so a server-sent
// event stream reaches the runtime as it arrives.
func streamResponse(w http.ResponseWriter, body io.Reader) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// mcpOAuthProxySet is the proxies of one run. Every request they serve runs
// under the set's context, so Close ends calls still in flight, open event
// streams included, rather than leaving them reaching the MCP server after
// the run.
type mcpOAuthProxySet struct {
	ctx       context.Context
	cancel    context.CancelFunc
	servers   []*http.Server
	listeners []net.Listener
	once      sync.Once
}

func newMCPOAuthProxySet() *mcpOAuthProxySet {
	ctx, cancel := context.WithCancel(context.Background())
	return &mcpOAuthProxySet{ctx: ctx, cancel: cancel}
}

// Close cancels every request in flight (which aborts its upstream call),
// lets the handlers return, then closes whatever connections remain.
func (s *mcpOAuthProxySet) Close() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		s.cancel()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		for _, server := range s.servers {
			if err := server.Shutdown(ctx); err != nil {
				_ = server.Close()
			}
			_ = server.Close()
		}
		for _, listener := range s.listeners {
			_ = listener.Close()
		}
	})
}

// mcpOAuthConfigUnreadableNotice is the transcript line when the run's MCP
// config could not be parsed to withhold the OAuth server names.
const mcpOAuthConfigUnreadableNotice = "No MCP servers were mounted for this run: its MCP configuration could not be read."

// mcpOAuthMountInput is what applyMCPOAuthServers needs about the run.
type mcpOAuthMountInput struct {
	Agent    *AgentData
	Provider string
	// Effective is the MCP config the run would use: the agent's config
	// merged with the runtime's own and any Remote MCP brokers. Empty
	// means the runtime would use its own config unmanaged.
	Effective json.RawMessage
	// RuntimeConfig merges the runtime's own MCP servers into a managed
	// config (mergeRuntimeAndAgentMcpConfig in production).
	RuntimeConfig func(json.RawMessage) (json.RawMessage, error)
	Fetch         mcpOAuthCredentialFetcher
	// Transport is the proxies' upstream transport; nil means the default.
	Transport http.RoundTripper
	Logger    *slog.Logger
}

// applyMCPOAuthServers makes the run's MCP config honor the server's OAuth
// MCP decisions:
//
//   - every OAuth server name is withheld from the whole config, including
//     the runtime's own MCP servers, so a same-named local entry (and its
//     static credential) is never used in its place;
//   - each server to mount is added as a local proxy entry; a runtime the
//     proxy is not offered to (outside the set the Remote MCP broker
//     supports) does not get the server at all, since the only alternative
//     is a token in its config file;
//   - when the server could not read the names (WithholdAll), no MCP server
//     is mounted at all.
//
// A config that was unmanaged becomes managed (the runtime's own servers,
// minus the withheld names), since an unmanaged runtime would load its own
// same-named entry. Servers that cannot be served are left out and returned
// as notices for the transcript. With nothing from the server it returns
// Effective unchanged.
func applyMCPOAuthServers(in mcpOAuthMountInput) (json.RawMessage, *mcpOAuthProxySet, []string) {
	agent := in.Agent
	if agent == nil || (!agent.MCPOAuthWithholdAll && len(agent.MCPOAuthNames) == 0 && len(agent.MCPOAuthProxy) == 0) {
		return in.Effective, nil, nil
	}
	if agent.MCPOAuthWithholdAll {
		return json.RawMessage(`{"mcpServers":{}}`), nil, nil
	}
	effective := in.Effective
	if trimmed := bytes.TrimSpace(effective); len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		effective = json.RawMessage(`{"mcpServers":{}}`)
		if in.RuntimeConfig != nil {
			if merged, err := in.RuntimeConfig(effective); err == nil && len(merged) > 0 {
				effective = merged
			}
		}
	}
	withheld := append([]string(nil), agent.MCPOAuthNames...)
	for _, server := range agent.MCPOAuthProxy {
		withheld = append(withheld, server.Name)
	}
	effective, err := withholdMCPServerNames(effective, withheld)
	if err != nil {
		// It cannot be checked for the withheld names, so none of it is
		// mounted; the transcript says so rather than tools going missing
		// without a word.
		in.Logger.Warn("MCP OAuth: run MCP config does not parse; mounting no MCP servers", "error", err)
		return json.RawMessage(`{"mcpServers":{}}`), nil, []string{mcpOAuthConfigUnreadableNotice}
	}

	set := newMCPOAuthProxySet()
	entries := map[string]any{}
	var notices []string
	for _, server := range agent.MCPOAuthProxy {
		if !providerSupportsRemoteMCPBroker(in.Provider) {
			// The local proxy is offered to the runtimes the Remote MCP
			// broker supports. Any other runtime would need the token in
			// its own config file, where it would outlive the run and
			// expire mid-task, so the server is not mounted for it.
			notices = append(notices, fmt.Sprintf("MCP server %q was not mounted: OAuth MCP servers are not supported with the %s runtime yet.", server.Name, in.Provider))
			continue
		}
		proxy, err := newMCPOAuthProxy(server, in.Fetch, in.Transport, in.Logger)
		if err != nil {
			notices = append(notices, fmt.Sprintf("MCP server %q was not mounted: its URL cannot be used.", server.Name))
			continue
		}
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			notices = append(notices, fmt.Sprintf("MCP server %q was not mounted this time: the local proxy could not start.", server.Name))
			continue
		}
		httpServer := &http.Server{
			Handler:           proxy,
			ReadHeaderTimeout: 10 * time.Second,
			BaseContext:       func(net.Listener) context.Context { return set.ctx },
		}
		set.servers = append(set.servers, httpServer)
		set.listeners = append(set.listeners, listener)
		go func() {
			if serveErr := httpServer.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				in.Logger.Warn("MCP OAuth proxy stopped unexpectedly", "server", server.Name, "error", serveErr)
			}
		}()
		entries[server.Name] = map[string]any{"type": "http", "url": "http://" + listener.Addr().String() + proxy.path}
	}
	if len(set.servers) == 0 {
		set.Close()
		set = nil
	}
	if len(entries) == 0 {
		return effective, set, notices
	}
	overlay, err := json.Marshal(map[string]any{"mcpServers": entries})
	if err == nil {
		var merged json.RawMessage
		if merged, err = mergeTaskRemoteMCPConfig(effective, overlay); err == nil {
			return merged, set, notices
		}
	}
	in.Logger.Warn("MCP OAuth: could not add the servers to the MCP config", "error", err)
	set.Close()
	for _, server := range agent.MCPOAuthProxy {
		if _, ok := entries[server.Name]; ok {
			notices = append(notices, fmt.Sprintf("MCP server %q was not mounted this time: the MCP configuration could not be combined with it.", server.Name))
		}
	}
	return effective, nil, notices
}

// withholdMCPServerNames removes the named entries from config's mcpServers,
// keeping every other top-level key.
func withholdMCPServerNames(config json.RawMessage, names []string) (json.RawMessage, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(config, &top); err != nil {
		return nil, err
	}
	if top == nil {
		top = map[string]json.RawMessage{}
	}
	servers := map[string]json.RawMessage{}
	if raw, ok := top["mcpServers"]; ok && len(bytes.TrimSpace(raw)) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if err := json.Unmarshal(raw, &servers); err != nil {
			return nil, err
		}
	}
	for _, name := range names {
		delete(servers, strings.TrimSpace(name))
	}
	rawServers, err := json.Marshal(servers)
	if err != nil {
		return nil, err
	}
	top["mcpServers"] = rawServers
	return json.Marshal(top)
}
