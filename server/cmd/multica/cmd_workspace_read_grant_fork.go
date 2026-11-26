// Added by the mouse9527/multica fork: cross-workspace read-only access (M-307).

package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

var workspaceReadGrantCmd = &cobra.Command{
	Use:   "read-grant",
	Short: "Let another workspace's agents read this workspace (owner/admin only)",
	Long: "A read grant lets the agents of another workspace (the source) read " +
		"this workspace with their own task tokens, by passing --workspace-id " +
		"<this workspace>. They get reads only: no writes, no chat, no private " +
		"projects, no secrets. Every read is audited. Needs the " +
		"cross_workspace_read feature on the server.",
}

var workspaceReadGrantListCmd = &cobra.Command{
	Use:   "list [workspace-id|slug|prefix]",
	Short: "List the read grants this workspace gives, revoked and expired ones included",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runWorkspaceReadGrantList,
}

var workspaceReadGrantCreateCmd = &cobra.Command{
	Use:   "create [workspace-id|slug|prefix]",
	Short: "Let one agent, or every agent, of another workspace read this workspace",
	Args:  cobra.MaximumNArgs(1),
	Example: "  multica workspace read-grant create --source-workspace <uuid> --agent <uuid> --expires-in 720h\n" +
		"  multica workspace read-grant create --source-workspace <uuid> --note \"platform troubleshooting\"",
	RunE: runWorkspaceReadGrantCreate,
}

var workspaceReadGrantRevokeCmd = &cobra.Command{
	Use:   "revoke <grant-id> [workspace-id|slug|prefix]",
	Short: "Revoke a read grant; the next request under it is refused",
	Args:  cobra.RangeArgs(1, 2),
	RunE:  runWorkspaceReadGrantRevoke,
}

var workspaceReadGrantAuditCmd = &cobra.Command{
	Use:   "audit [workspace-id|slug|prefix]",
	Short: "Show the newest reads made under this workspace's grants",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runWorkspaceReadGrantAudit,
}

func init() {
	workspaceCmd.AddCommand(workspaceReadGrantCmd)
	workspaceReadGrantCmd.AddCommand(workspaceReadGrantListCmd)
	workspaceReadGrantCmd.AddCommand(workspaceReadGrantCreateCmd)
	workspaceReadGrantCmd.AddCommand(workspaceReadGrantRevokeCmd)
	workspaceReadGrantCmd.AddCommand(workspaceReadGrantAuditCmd)

	workspaceReadGrantListCmd.Flags().String("output", "json", "Output format: table or json")
	workspaceReadGrantCreateCmd.Flags().String("source-workspace", "", "ID of the workspace whose agents may read (required)")
	workspaceReadGrantCreateCmd.Flags().String("agent", "", "ID of the one agent of the source workspace that may read (default: every agent)")
	workspaceReadGrantCreateCmd.Flags().String("expires-at", "", "When the grant ends, RFC 3339 (default: until revoked)")
	workspaceReadGrantCreateCmd.Flags().Duration("expires-in", 0, "How long the grant lasts, e.g. 720h (default: until revoked)")
	workspaceReadGrantCreateCmd.Flags().String("note", "", "Why the grant exists")
	workspaceReadGrantCreateCmd.Flags().String("output", "json", "Output format: table or json")
	workspaceReadGrantRevokeCmd.Flags().String("output", "json", "Output format: table or json")
	workspaceReadGrantAuditCmd.Flags().Int("limit", 50, "How many entries to show (at most 500)")
	workspaceReadGrantAuditCmd.Flags().String("output", "json", "Output format: table or json")
}

// readGrant is the CLI's view of a grant.
type readGrant struct {
	ID                string  `json:"id"`
	TargetWorkspaceID string  `json:"target_workspace_id"`
	SourceWorkspaceID string  `json:"source_workspace_id"`
	AgentID           string  `json:"agent_id,omitempty"`
	Note              string  `json:"note"`
	CreatedBy         string  `json:"created_by"`
	CreatedAt         string  `json:"created_at"`
	ExpiresAt         *string `json:"expires_at"`
	RevokedAt         *string `json:"revoked_at"`
	RevokedBy         string  `json:"revoked_by,omitempty"`
}

type readGrantAudit struct {
	ID                string `json:"id"`
	GrantID           string `json:"grant_id"`
	TargetWorkspaceID string `json:"target_workspace_id"`
	SourceWorkspaceID string `json:"source_workspace_id"`
	AgentID           string `json:"agent_id"`
	TaskID            string `json:"task_id"`
	Method            string `json:"method"`
	Route             string `json:"route"`
	Allowed           bool   `json:"allowed"`
	CreatedAt         string `json:"created_at"`
}

func readGrantWorkspace(cmd *cobra.Command, args []string) (string, error) {
	wsID, err := resolveWorkspaceArg(cmd, args)
	if err != nil {
		return "", err
	}
	if wsID == "" {
		return "", fmt.Errorf("workspace ID is required: pass an id/slug/prefix as argument or set MULTICA_WORKSPACE_ID")
	}
	return wsID, nil
}

func readGrantsPath(wsID string) string {
	return "/api/workspaces/" + url.PathEscape(wsID) + "/cross-read-grants"
}

func runWorkspaceReadGrantList(cmd *cobra.Command, args []string) error {
	wsID, err := readGrantWorkspace(cmd, args)
	if err != nil {
		return err
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var grants []readGrant
	if err := client.GetJSON(ctx, readGrantsPath(wsID), &grants); err != nil {
		return fmt.Errorf("list read grants: %w", err)
	}
	return printReadGrants(cmd, grants)
}

func runWorkspaceReadGrantCreate(cmd *cobra.Command, args []string) error {
	source, _ := cmd.Flags().GetString("source-workspace")
	source = strings.TrimSpace(source)
	if !uuidRegexp.MatchString(source) {
		return fmt.Errorf("--source-workspace must be a workspace ID")
	}
	body := map[string]any{"source_workspace_id": source}
	if agent, _ := cmd.Flags().GetString("agent"); strings.TrimSpace(agent) != "" {
		if !uuidRegexp.MatchString(strings.TrimSpace(agent)) {
			return fmt.Errorf("--agent must be an agent ID")
		}
		body["agent_id"] = strings.TrimSpace(agent)
	}
	expiresAt, _ := cmd.Flags().GetString("expires-at")
	expiresIn, _ := cmd.Flags().GetDuration("expires-in")
	switch {
	case strings.TrimSpace(expiresAt) != "" && expiresIn != 0:
		return fmt.Errorf("pass --expires-at or --expires-in, not both")
	case strings.TrimSpace(expiresAt) != "":
		body["expires_at"] = strings.TrimSpace(expiresAt)
	case expiresIn < 0:
		return fmt.Errorf("--expires-in must be positive")
	case expiresIn > 0:
		body["expires_at"] = time.Now().Add(expiresIn).UTC().Format(time.RFC3339)
	}
	if note, _ := cmd.Flags().GetString("note"); strings.TrimSpace(note) != "" {
		body["note"] = strings.TrimSpace(note)
	}

	wsID, err := readGrantWorkspace(cmd, args)
	if err != nil {
		return err
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var grant readGrant
	if err := client.PostJSON(ctx, readGrantsPath(wsID), body, &grant); err != nil {
		return fmt.Errorf("create read grant: %w", err)
	}
	return printReadGrants(cmd, []readGrant{grant})
}

func runWorkspaceReadGrantRevoke(cmd *cobra.Command, args []string) error {
	grantID := strings.TrimSpace(args[0])
	if !uuidRegexp.MatchString(grantID) {
		return fmt.Errorf("grant ID must be a UUID")
	}
	wsID, err := readGrantWorkspace(cmd, args[1:])
	if err != nil {
		return err
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var grant readGrant
	if err := client.DeleteJSONResponse(ctx, readGrantsPath(wsID)+"/"+url.PathEscape(grantID), &grant); err != nil {
		return fmt.Errorf("revoke read grant: %w", err)
	}
	return printReadGrants(cmd, []readGrant{grant})
}

func runWorkspaceReadGrantAudit(cmd *cobra.Command, args []string) error {
	wsID, err := readGrantWorkspace(cmd, args)
	if err != nil {
		return err
	}
	limit, _ := cmd.Flags().GetInt("limit")
	if limit <= 0 {
		return fmt.Errorf("--limit must be positive")
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var rows []readGrantAudit
	if err := client.GetJSON(ctx, readGrantsPath(wsID)+"/audit?limit="+strconv.Itoa(limit), &rows); err != nil {
		return fmt.Errorf("list read audit: %w", err)
	}
	output, _ := cmd.Flags().GetString("output")
	if output != "table" {
		return cli.PrintJSON(os.Stdout, rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(os.Stdout, "no reads")
		return nil
	}
	table := make([][]string, 0, len(rows))
	for _, r := range rows {
		result := "allowed"
		if !r.Allowed {
			result = "refused"
		}
		table = append(table, []string{r.CreatedAt, r.AgentID, r.TaskID, r.Route, result})
	}
	cli.PrintTable(os.Stdout, []string{"TIME", "AGENT", "TASK", "ROUTE", "RESULT"}, table)
	return nil
}

func printReadGrants(cmd *cobra.Command, grants []readGrant) error {
	output, _ := cmd.Flags().GetString("output")
	if output != "table" {
		return cli.PrintJSON(os.Stdout, grants)
	}
	if len(grants) == 0 {
		fmt.Fprintln(os.Stdout, "no read grants")
		return nil
	}
	rows := make([][]string, 0, len(grants))
	for _, g := range grants {
		agent := g.AgentID
		if agent == "" {
			agent = "all agents"
		}
		state := "active"
		switch {
		case g.RevokedAt != nil:
			state = "revoked"
		case g.ExpiresAt != nil:
			state = "until " + *g.ExpiresAt
		}
		rows = append(rows, []string{g.ID, g.SourceWorkspaceID, agent, state, g.Note})
	}
	cli.PrintTable(os.Stdout, []string{"ID", "SOURCE WORKSPACE", "AGENT", "STATE", "NOTE"}, rows)
	return nil
}
