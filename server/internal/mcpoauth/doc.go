// Added by the mouse9527/multica fork: server-hosted OAuth 2.1 MCP (M-230).

// Package mcpoauth lets an agent use an OAuth 2.1 protected MCP server without
// an interactive login on the daemon machine.
//
// The agent's owner connects the server once in the web app. The server runs
// the authorization code flow with PKCE, keeps the resulting tokens sealed
// with secretbox, and at claim time hands the daemon a short-lived access
// token as an ordinary `{type: "http", headers: {Authorization: ...}}` MCP
// entry. Tokens are never written to agent_task_queue and never returned by
// the API.
//
// This package owns the storage (migration 9023), the one-time authorization
// state, and on-demand refresh (TokenSource). Refresh is serialized per
// connection with a row lock, because authorization servers that rotate
// refresh tokens invalidate the old one on use: two concurrent refreshes with
// the same refresh token would leave one of them holding a dead token, and
// some servers revoke the whole grant on reuse.
//
// The OAuth protocol calls themselves (discovery, registration, code exchange,
// refresh) come from pkg/remotemcp, which this package does not modify.
package mcpoauth
