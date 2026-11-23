// Added by the mouse9527/multica fork: which Super Agents cover an issue, the
// scope hint on `issue create` in a Super Agent chat, and the overlap refusal
// on subscribe (M-194).

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

var routerCoveringCmd = &cobra.Command{
	Use:   "covering <issue>",
	Short: "List the Super Agents whose scope contains an issue, and which of them receive its events",
	Long: "List the subscriptions whose scope contains an issue (key or UUID).\n\n" +
		"An agent receives an issue's events from one subscription only, its narrowest: an issue tree beats a project, " +
		"and a tree rooted closer to the issue beats one further up. DELIVERS says which subscription that is; " +
		"the agent's wider ones are marked \"no (narrower <id>)\". Different agents each receive the events. " +
		"Chats are named only to the member who started them.",
	Args: exactArgs(1),
	RunE: runRouterCovering,
}

func init() {
	routerCmd.AddCommand(routerCoveringCmd)
	routerCoveringCmd.Flags().String("output", "table", "Output format: table or json")
	routerCoveringCmd.Flags().Bool("full-id", false, "Show full UUIDs in table output")
}

func runRouterCovering(cmd *cobra.Command, args []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	ref, err := resolveIssueRef(ctx, client, args[0])
	if err != nil {
		return fmt.Errorf("resolve issue: %w", err)
	}
	var result struct {
		IssueID  string           `json:"issue_id"`
		Covering []map[string]any `json:"covering"`
	}
	if err := client.GetJSON(ctx, "/api/router/issues/"+url.PathEscape(ref.ID)+"/covering", &result); err != nil {
		return fmt.Errorf("list covering subscriptions: %w", err)
	}
	if result.Covering == nil {
		result.Covering = []map[string]any{}
	}
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, result)
	}
	if len(result.Covering) == 0 {
		fmt.Fprintf(os.Stderr, "No Super Agent subscription covers %s.\n", args[0])
	}
	fullID, _ := cmd.Flags().GetBool("full-id")
	cli.PrintTable(os.Stdout, []string{"SUBSCRIPTION", "AGENT", "SCOPE", "CHAT", "STATE", "DELIVERS"},
		routerCoveringRows(result.Covering, fullID))
	return nil
}

func routerCoveringRows(covering []map[string]any, fullID bool) [][]string {
	rows := make([][]string, 0, len(covering))
	for _, c := range covering {
		agent := strVal(c, "agent_name")
		if agent == "" {
			agent = displayID(strVal(c, "agent_id"), fullID)
		}
		scope := strVal(c, "scope_name")
		if scope == "" {
			scope = displayID(strVal(c, "scope_id"), fullID)
		}
		chat := "-"
		if bound, _ := c["chat_bound"].(bool); bound {
			chat = "(another member's chat)"
			if id := strVal(c, "chat_session_id"); id != "" {
				chat = displayID(id, fullID)
				if title := strVal(c, "chat_title"); title != "" {
					chat = title + " " + chat
				}
			}
		}
		state := "running"
		if enabled, _ := c["enabled"].(bool); !enabled {
			state = "paused"
		} else if reason := strVal(c, "paused_reason"); reason != "" {
			state = "dispatch paused (" + reason + ")"
		}
		delivers := "no"
		if effective, _ := c["effective"].(bool); effective {
			delivers = "yes"
		} else if by := strVal(c, "superseded_by"); by != "" {
			delivers = "no (narrower " + displayID(by, fullID) + ")"
		}
		rows = append(rows, []string{
			displayID(strVal(c, "subscription_id"), fullID),
			agent,
			strVal(c, "scope_type") + ":" + scope,
			chat,
			state,
			delivers,
		})
	}
	return rows
}

// printSuperAgentScopeHint tells the agent on stderr when an issue it just
// created from a Super Agent chat will not reach that chat. The server adds
// super_agent_scope only to creates from a Super Agent chat run.
func printSuperAgentScopeHint(w io.Writer, issue map[string]any) {
	scope, ok := issue["super_agent_scope"].(map[string]any)
	if !ok {
		return
	}
	if inScope, _ := scope["in_scope"].(bool); inScope {
		return
	}
	if hint := strVal(scope, "hint"); hint != "" {
		fmt.Fprintf(w, "Super Agent: %s\n", hint)
	}
}

// routerOverlapMessage turns the server's overlap refusal into a message that
// names the overlapping Super Agents and how to subscribe anyway.
func routerOverlapMessage(err error) (string, bool) {
	var httpErr *cli.HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusConflict {
		return "", false
	}
	var payload struct {
		Code     string `json:"code"`
		Overlaps []struct {
			SubscriptionID string   `json:"subscription_id"`
			AgentID        string   `json:"agent_id"`
			AgentName      string   `json:"agent_name"`
			ScopeType      string   `json:"scope_type"`
			ScopeID        string   `json:"scope_id"`
			ScopeName      string   `json:"scope_name"`
			IssueCount     int      `json:"issue_count"`
			Issues         []string `json:"issues"`
		} `json:"overlaps"`
	}
	if json.Unmarshal([]byte(httpErr.Body), &payload) != nil || payload.Code != "router_scope_overlap" {
		return "", false
	}
	var b strings.Builder
	b.WriteString("other Super Agents already receive events from this scope, so each would act on the same issues:\n")
	for _, o := range payload.Overlaps {
		agent := o.AgentName
		if agent == "" {
			agent = o.AgentID
		}
		scope := o.ScopeName
		if scope == "" {
			scope = o.ScopeID
		}
		fmt.Fprintf(&b, "  - %s, subscription %s (%s %s): %d issue(s)", agent, o.SubscriptionID, o.ScopeType, scope, o.IssueCount)
		if len(o.Issues) > 0 {
			b.WriteString(", e.g. " + strings.Join(o.Issues, ", "))
		}
		b.WriteString("\n")
	}
	b.WriteString("Check with the owner (`multica router covering <issue>` shows who covers an issue); " +
		"only after they agree, repeat the command with --allow-overlap")
	return b.String(), true
}
