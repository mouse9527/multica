// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
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
// running task from the Multica server.
type mcpOAuthCredentialFetcher func(ctx context.Context, serverID string) (mcpOAuthCredential, error)

const (
	// mcpOAuthProxyMaxBody bounds one MCP request body, which the proxy
	// buffers so it can resend it once with a fresh token after a 401.
	mcpOAuthProxyMaxBody = 4 << 20
	// mcpOAuthCredentialSlack: a cached token that expires within it is
	// fetched again before use.
	mcpOAuthCredentialSlack        = time.Minute
	mcpOAuthCredentialFetchTimeout = 20 * time.Second
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

func newMCPOAuthProxy(server MCPOAuthProxyServerData, fetch mcpOAuthCredentialFetcher, logger *slog.Logger) (*mcpOAuthProxy, error) {
	target, err := url.Parse(server.URL)
	if err != nil || (target.Scheme != "https" && target.Scheme != "http") || target.Host == "" {
		return nil, fmt.Errorf("MCP OAuth server %q has an unusable URL", server.Name)
	}
	pathToken := make([]byte, 24)
	if _, err := rand.Read(pathToken); err != nil {
		return nil, err
	}
	transport := httptransport.CloneDefault().(*http.Transport)
	// MCP over HTTP answers with a stream for as long as the call runs, so
	// there is no overall client timeout; only the wait for headers is
	// bounded.
	transport.ResponseHeaderTimeout = 2 * time.Minute
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
// none, it is about to expire, or fresh is set (after a 401).
func (p *mcpOAuthProxy) credential(ctx context.Context, fresh bool) (mcpOAuthCredential, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !fresh && p.cached != nil && (p.cached.ExpiresAt.IsZero() || time.Until(p.cached.ExpiresAt) > mcpOAuthCredentialSlack) {
		return *p.cached, nil
	}
	fetchCtx, cancel := context.WithTimeout(ctx, mcpOAuthCredentialFetchTimeout)
	defer cancel()
	credential, err := p.fetch(fetchCtx, p.serverID)
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
	response, err := p.forward(r, body, false)
	if err == nil && response.StatusCode == http.StatusUnauthorized {
		// The token may have been revoked or rotated since it was cached:
		// try once more with a fresh one.
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, mcpOAuthProxyMaxBody))
		response.Body.Close()
		response, err = p.forward(r, body, true)
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

func (p *mcpOAuthProxy) forward(r *http.Request, body []byte, fresh bool) (*http.Response, error) {
	credential, err := p.credential(r.Context(), fresh)
	if err != nil {
		return nil, fmt.Errorf("credential: %w", err)
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
		return nil, err
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
	return p.client.Do(outbound)
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

// mcpOAuthProxySet is the proxies of one run.
type mcpOAuthProxySet struct {
	servers   []*http.Server
	listeners []net.Listener
	once      sync.Once
}

func (s *mcpOAuthProxySet) Close() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		for _, server := range s.servers {
			_ = server.Shutdown(ctx)
		}
		for _, listener := range s.listeners {
			_ = listener.Close()
		}
	})
}

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
	Logger        *slog.Logger
}

// applyMCPOAuthServers makes the run's MCP config honor the server's OAuth
// MCP decisions:
//
//   - every OAuth server name is withheld from the whole config, including
//     the runtime's own MCP servers, so a same-named local entry (and its
//     static credential) is never used in its place;
//   - each server to mount is added as a local proxy entry, or, for a
//     runtime the proxy is not offered to (the same set the Remote MCP
//     broker supports), as a direct entry with the token fetched now;
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
		in.Logger.Warn("MCP OAuth: run MCP config does not parse; mounting no MCP servers", "error", err)
		return json.RawMessage(`{"mcpServers":{}}`), nil, nil
	}

	set := &mcpOAuthProxySet{}
	entries := map[string]any{}
	var notices []string
	for _, server := range agent.MCPOAuthProxy {
		if !providerSupportsRemoteMCPBroker(in.Provider) {
			// No local proxy for this runtime: give it the current token
			// directly, as a daemon without the proxy would have got it.
			ctx, cancel := context.WithTimeout(context.Background(), mcpOAuthCredentialFetchTimeout)
			credential, fetchErr := in.Fetch(ctx, server.ServerID)
			cancel()
			if fetchErr != nil {
				notices = append(notices, fmt.Sprintf("MCP server %q was not mounted this time: a current token could not be obtained. The next task will try again.", server.Name))
				continue
			}
			entries[server.Name] = map[string]any{"type": "http", "url": server.URL, "headers": credential.Headers}
			continue
		}
		proxy, err := newMCPOAuthProxy(server, in.Fetch, in.Logger)
		if err != nil {
			notices = append(notices, fmt.Sprintf("MCP server %q was not mounted: its URL cannot be used.", server.Name))
			continue
		}
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			notices = append(notices, fmt.Sprintf("MCP server %q was not mounted this time: the local proxy could not start.", server.Name))
			continue
		}
		httpServer := &http.Server{Handler: proxy, ReadHeaderTimeout: 10 * time.Second}
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
