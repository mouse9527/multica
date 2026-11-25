// Added by the mouse9527/multica fork: OAuth as an auth mode of MCP entries (M-269).

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/mcpoauth"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Entry sources: where the entry an agent runs with comes from.
const (
	mcpOAuthSourceAgent     = "agent"
	mcpOAuthSourceWorkspace = "workspace"
)

// mcpOAuthEntry is one OAuth entry of the MCP config an agent runs with.
type mcpOAuthEntry struct {
	Name string
	// Ref is the connection key (mcpoauth.WorkspaceRef / AgentRef); empty
	// when the entry's source could not be determined this time.
	Ref               string
	Source            string
	WorkspaceServerID string
	Entry             mcpoauth.Entry
	// Err is why the entry is not a usable OAuth entry (mcpoauth.ParseEntry,
	// or a name that cannot be part of an agent ref).
	Err error
}

var errMCPOAuthEntryName = errors.New("the name of an OAuth entry must be 1 to 64 letters, digits, '-' or '_'")

// mcpServersOf splits an mcp_config document into its top-level keys and its
// servers, read across both containers the way ResolveAgentMcpConfig folds
// them: the legacy "mcp" first, "mcpServers" over it. ok is false for an
// absent document or one that does not parse.
func mcpServersOf(config json.RawMessage) (doc, servers map[string]json.RawMessage, ok bool) {
	if !hasManagedJSON(config) {
		return nil, nil, false
	}
	if err := json.Unmarshal(config, &doc); err != nil || doc == nil {
		return nil, nil, false
	}
	servers = map[string]json.RawMessage{}
	for _, container := range [...]string{"mcp", "mcpServers"} {
		own, err := unmarshalServerMap(doc[container])
		if err != nil {
			continue
		}
		for name, entry := range own {
			servers[name] = entry
		}
	}
	return doc, servers, true
}

// mcpOAuthEntriesIn returns the OAuth entries among servers, by name, parsed
// but without their source.
func mcpOAuthEntriesIn(servers map[string]json.RawMessage) []mcpOAuthEntry {
	var out []mcpOAuthEntry
	for name, raw := range servers {
		if !mcpoauth.IsOAuthEntry(raw) {
			continue
		}
		entry, err := mcpoauth.ParseEntry(raw)
		out = append(out, mcpOAuthEntry{Name: name, Entry: entry, Err: err})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// agentMcpBindings loads the workspace entries bound to an agent once, on
// first use.
type agentMcpBindings struct {
	h       *Handler
	agentID pgtype.UUID
	rows    []db.ListAgentMcpServersRow
	err     error
	loaded  bool
}

func (b *agentMcpBindings) enabled(ctx context.Context) ([]db.ListAgentMcpServersRow, error) {
	if !b.loaded {
		b.loaded = true
		rows, err := b.h.Queries.ListAgentMcpServers(ctx, b.agentID)
		if err != nil {
			b.err = err
		}
		for _, row := range rows {
			if row.Enabled {
				b.rows = append(b.rows, row)
			}
		}
	}
	return b.rows, b.err
}

// resolveMCPOAuthSources sets each entry's source and ref. The agent's own
// mcp_config wins a name over a bound workspace entry (ResolveAgentMcpConfig),
// so a name the agent declares is the agent's entry; any other name is the
// enabled workspace entry bound under it. An entry found in neither (the
// bindings changed or could not be read since the config was assembled) keeps
// an empty ref: it gets no credential.
func resolveMCPOAuthSources(ctx context.Context, agent db.Agent, bindings *agentMcpBindings, entries []mcpOAuthEntry) {
	_, own, _ := mcpServersOf(agent.McpConfig)
	agentID := uuidToString(agent.ID)
	for i := range entries {
		entry := &entries[i]
		if _, mine := own[entry.Name]; mine {
			entry.Source = mcpOAuthSourceAgent
			if !mcpoauth.ValidEntryName(entry.Name) {
				if entry.Err == nil {
					entry.Err = errMCPOAuthEntryName
				}
				continue
			}
			entry.Ref = mcpoauth.AgentRef(agentID, entry.Name)
			continue
		}
		rows, err := bindings.enabled(ctx)
		if err != nil {
			continue
		}
		for _, row := range rows {
			if row.Name == entry.Name {
				entry.Source = mcpOAuthSourceWorkspace
				entry.WorkspaceServerID = uuidToString(row.ID)
				entry.Ref = mcpoauth.WorkspaceRef(entry.WorkspaceServerID)
				break
			}
		}
	}
}

// agentMCPOAuthEntries returns the OAuth entries of the MCP config the agent
// runs with — its own mcp_config folded over its enabled workspace entries,
// as a claim assembles it — with their sources.
func (h *Handler) agentMCPOAuthEntries(ctx context.Context, agent db.Agent) ([]mcpOAuthEntry, error) {
	bindings := &agentMcpBindings{h: h, agentID: agent.ID}
	rows, err := bindings.enabled(ctx)
	if err != nil {
		return nil, err
	}
	bound := make([]WorkspaceMcpBinding, 0, len(rows))
	for _, row := range rows {
		bound = append(bound, WorkspaceMcpBinding{Name: row.Name, Config: json.RawMessage(row.Config)})
	}
	merged, err := ResolveAgentMcpConfig(bound, json.RawMessage(agent.McpConfig))
	if err != nil {
		// As on a claim: a malformed agent config leaves it unchanged.
		merged = passthroughAgentMcpConfig(json.RawMessage(agent.McpConfig))
	}
	_, servers, ok := mcpServersOf(merged)
	if !ok {
		return nil, nil
	}
	entries := mcpOAuthEntriesIn(servers)
	resolveMCPOAuthSources(ctx, agent, bindings, entries)
	return entries, nil
}

// validateMCPOAuthAgentConfig is the save hook for an agent's mcp_config:
// every entry that declares "auth" must be a valid OAuth entry. With
// mcp_oauth_v1 off nothing is checked, as upstream.
func (h *Handler) validateMCPOAuthAgentConfig(ctx context.Context, raw json.RawMessage) error {
	if !featureflags.MCPOAuthV1Enabled(ctx, h.FeatureFlags) {
		return nil
	}
	return mcpoauth.ValidateConfigForSave(ctx, raw)
}

// validateMCPOAuthWorkspaceEntry is the same hook for a workspace library
// entry.
func (h *Handler) validateMCPOAuthWorkspaceEntry(ctx context.Context, raw json.RawMessage) error {
	if !featureflags.MCPOAuthV1Enabled(ctx, h.FeatureFlags) {
		return nil
	}
	return mcpoauth.ValidateEntryForSave(ctx, raw)
}

// forgetRemovedAgentMCPOAuthEntries runs after an agent's mcp_config was
// written: connections to the agent's own entries that are gone, renamed or
// no longer use OAuth are deleted with their tokens. It needs no key and
// never fails the write; a failure is logged.
func (h *Handler) forgetRemovedAgentMCPOAuthEntries(ctx context.Context, agent db.Agent) {
	agentID := uuidToString(agent.ID)
	keep := []string{}
	if _, own, ok := mcpServersOf(agent.McpConfig); ok {
		for name, raw := range own {
			if mcpoauth.IsOAuthEntry(raw) && mcpoauth.ValidEntryName(name) {
				keep = append(keep, mcpoauth.AgentRef(agentID, name))
			}
		}
	}
	if err := mcpoauth.DeleteAgentEntryDataExcept(ctx, h.DB, uuidToString(agent.WorkspaceID), agentID, keep); err != nil {
		slog.Warn("mcp oauth: forget removed agent entries failed", "agent_id", agentID, "error", err)
	}
}

// forgetWorkspaceMCPOAuthEntry deletes every user's connection to a workspace
// entry being deleted, inside the delete's transaction.
func forgetWorkspaceMCPOAuthEntry(ctx context.Context, q mcpoauth.Querier, workspaceID, serverID pgtype.UUID) error {
	return mcpoauth.DeleteEntryData(ctx, q, uuidToString(workspaceID), mcpoauth.WorkspaceRef(uuidToString(serverID)))
}

// forgetUnboundMCPOAuthConnection runs after a workspace entry was unbound
// from an agent. The agent ran with its owner's connection to the entry; once
// none of the owner's agents is bound to it any more, that connection is
// deleted. It never fails the unbind; a failure is logged.
func (h *Handler) forgetUnboundMCPOAuthConnection(ctx context.Context, agent db.Agent, serverID pgtype.UUID) {
	if !agent.OwnerID.Valid {
		return
	}
	var stillBound bool
	if err := h.DB.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM agent_mcp_server ams
			JOIN agent a ON a.id = ams.agent_id
			WHERE ams.server_id = $1 AND a.owner_id = $2 AND a.workspace_id = $3
		)`, serverID, agent.OwnerID, agent.WorkspaceID).Scan(&stillBound); err != nil {
		slog.Warn("mcp oauth: check remaining bindings failed", "server_id", uuidToString(serverID), "error", err)
		return
	}
	if stillBound {
		return
	}
	if _, err := mcpoauth.DeleteUserConnection(ctx, h.DB, uuidToString(agent.WorkspaceID),
		mcpoauth.WorkspaceRef(uuidToString(serverID)), uuidToString(agent.OwnerID)); err != nil {
		slog.Warn("mcp oauth: forget unbound connection failed", "server_id", uuidToString(serverID), "error", err)
	}
}
