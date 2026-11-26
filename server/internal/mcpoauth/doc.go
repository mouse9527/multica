// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).
// Modified for the mouse9527/multica fork: OAuth as an auth mode of MCP entries (M-269).

// Package mcpoauth lets an agent use an OAuth 2.1 protected MCP server without
// an interactive login on the daemon machine.
//
// OAuth is an auth mode of an ordinary MCP entry, not a separate kind of
// server: an entry in an agent's mcp_config or in the workspace MCP library
// declares `"auth": {"type": "oauth"}` next to its URL (entry.go). A user
// connects the entry once in the web app; the server runs the authorization
// code flow with PKCE, keeps the resulting tokens sealed with secretbox, and
// at claim time hands the daemon a short-lived access token as an ordinary
// `{type: "http", headers: {Authorization: ...}}` MCP entry, or, for a daemon
// that proxies, fetches one per call. Tokens are never written to
// agent_task_queue, never put in an MCP config document, and never returned
// by the API.
//
// A connection is keyed by the entry's ref and the user (store.go): a
// workspace entry is connected once per user and shared by all of that
// user's agents it is bound to; an agent's own entry is connected per agent.
// Every connection remembers the URL it was made for, and a token is only
// ever handed out for that URL (TokenSource).
//
// This package owns the storage (migrations 9023, 9026 and 9027), the one-time
// authorization state, and on-demand refresh (TokenSource). Refresh is
// serialized per connection with a row lock, because authorization servers
// that rotate refresh tokens invalidate the old one on use: two concurrent
// refreshes with the same refresh token would leave one of them holding a
// dead token, and some servers revoke the whole grant on reuse.
//
// The OAuth protocol calls themselves (discovery, registration, code exchange,
// refresh) come from pkg/remotemcp, which this package does not modify.
package mcpoauth
