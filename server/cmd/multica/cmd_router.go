// Modified for the mouse9527/multica fork: Router MVP, slice S1 (M-85).

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

// Router commands: subscriptions that capture issue events for an agent, and
// the captured events. The server answers 404 while router_v1 is off.

var routerCmd = &cobra.Command{
	Use:   "router",
	Short: "Work with Router subscriptions and captured events",
}

var routerSubscribeCmd = &cobra.Command{
	Use:   "subscribe",
	Short: "Subscribe an agent to an issue tree (--issue) or a project (--project)",
	Args:  exactArgs(0),
	RunE:  runRouterSubscribe,
}

var routerListCmd = &cobra.Command{
	Use:   "list",
	Short: "List subscriptions in the workspace",
	Args:  exactArgs(0),
	RunE:  runRouterList,
}

var routerGetCmd = &cobra.Command{
	Use:   "get <subscription-id>",
	Short: "Get a subscription, including its capture_miss count",
	Args:  exactArgs(1),
	RunE:  runRouterGet,
}

var routerPauseCmd = &cobra.Command{
	Use:   "pause <subscription-id>",
	Short: "Stop capturing events for a subscription",
	Args:  exactArgs(1),
	RunE:  func(cmd *cobra.Command, args []string) error { return runRouterAction(cmd, args[0], "pause") },
}

var routerResumeCmd = &cobra.Command{
	Use:   "resume <subscription-id>",
	Short: "Resume capturing events for a subscription, starting from now",
	Args:  exactArgs(1),
	RunE:  func(cmd *cobra.Command, args []string) error { return runRouterAction(cmd, args[0], "resume") },
}

var routerDeleteCmd = &cobra.Command{
	Use:   "delete <subscription-id>",
	Short: "Delete a subscription and its captured events",
	Args:  exactArgs(1),
	RunE:  runRouterDelete,
}

var routerEventsCmd = &cobra.Command{
	Use:   "events",
	Short: "List captured events, oldest first",
	Args:  exactArgs(0),
	RunE:  runRouterEvents,
}

var routerScopeCmd = &cobra.Command{
	Use:   "scope <subscription-id>",
	Short: "List the issues a subscription currently covers",
	Args:  exactArgs(1),
	RunE:  runRouterScope,
}

func init() {
	rootCmd.AddCommand(routerCmd)
	routerCmd.AddCommand(routerSubscribeCmd, routerListCmd, routerGetCmd, routerPauseCmd,
		routerResumeCmd, routerDeleteCmd, routerEventsCmd, routerScopeCmd)

	routerSubscribeCmd.Flags().String("agent", "", "Agent name or ID (default: the calling agent)")
	routerSubscribeCmd.Flags().String("issue", "", "Root issue of the subscribed tree (key or UUID)")
	routerSubscribeCmd.Flags().String("project", "", "Subscribed project (ID)")
	routerSubscribeCmd.Flags().StringSlice("events", nil, "Event types to capture (default: all): issue_created, status_changed, assignee_changed, comment_created, task_failed")
	routerSubscribeCmd.Flags().String("output", "json", "Output format: table or json")

	routerListCmd.Flags().String("agent", "", "Only this agent's subscriptions (name or ID)")
	routerListCmd.Flags().String("output", "table", "Output format: table or json")
	routerListCmd.Flags().Bool("full-id", false, "Show full UUIDs in table output")

	routerGetCmd.Flags().String("output", "json", "Output format: table or json")
	routerPauseCmd.Flags().String("output", "json", "Output format: table or json")
	routerResumeCmd.Flags().String("output", "json", "Output format: table or json")

	routerEventsCmd.Flags().String("subscription", "", "Only this subscription's events (ID or prefix)")
	routerEventsCmd.Flags().String("agent", "", "Only this agent's events (name or ID)")
	routerEventsCmd.Flags().String("since", "", "Events captured after this time: RFC 3339, or a duration such as 2h")
	routerEventsCmd.Flags().String("state", "", "pending or consumed")
	routerEventsCmd.Flags().String("source", "", "bus (captured live) or reconcile (missed by the listener)")
	routerEventsCmd.Flags().Int("limit", 0, "Maximum events to return (1-500, default 100)")
	routerEventsCmd.Flags().String("output", "table", "Output format: table or json")

	routerScopeCmd.Flags().Bool("all", false, "Include done and cancelled issues")
	routerScopeCmd.Flags().String("output", "table", "Output format: table or json")
}

func routerSubscriptionPath(id string) string {
	return "/api/router/subscriptions/" + url.PathEscape(id)
}

func resolveRouterSubscriptionID(ctx context.Context, client *cli.APIClient, input string) (string, error) {
	ref, err := resolveIDByPrefix(ctx, client, "subscription", input, func(ctx context.Context, client *cli.APIClient) ([]idCandidate, error) {
		var result struct {
			Subscriptions []map[string]any `json:"subscriptions"`
		}
		if err := client.GetJSON(ctx, "/api/router/subscriptions", &result); err != nil {
			return nil, err
		}
		out := make([]idCandidate, 0, len(result.Subscriptions))
		for _, s := range result.Subscriptions {
			out = append(out, idCandidate{ID: strVal(s, "id"), Display: strVal(s, "scope_type") + " " + strVal(s, "scope_id")})
		}
		return out, nil
	})
	return ref.ID, err
}

func runRouterSubscribe(cmd *cobra.Command, _ []string) error {
	issue, _ := cmd.Flags().GetString("issue")
	project, _ := cmd.Flags().GetString("project")
	if (issue == "") == (project == "") {
		return fmt.Errorf("pass exactly one of --issue or --project")
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	body := map[string]any{}
	if agent, _ := cmd.Flags().GetString("agent"); agent != "" {
		agentID, err := resolveAgent(ctx, client, agent)
		if err != nil {
			return fmt.Errorf("resolve agent: %w", err)
		}
		body["agent_id"] = agentID
	}
	if issue != "" {
		ref, err := resolveIssueRef(ctx, client, issue)
		if err != nil {
			return fmt.Errorf("resolve issue: %w", err)
		}
		body["scope_type"], body["scope_id"] = "issue_tree", ref.ID
	} else {
		ref, err := resolveProjectID(ctx, client, project)
		if err != nil {
			return fmt.Errorf("resolve project: %w", err)
		}
		body["scope_type"], body["scope_id"] = "project", ref.ID
	}
	if events, _ := cmd.Flags().GetStringSlice("events"); len(events) > 0 {
		body["event_types"] = events
	}

	var sub map[string]any
	if err := client.PostJSON(ctx, "/api/router/subscriptions", body, &sub); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	return printRouterSubscription(cmd, sub)
}

func runRouterList(cmd *cobra.Command, _ []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	params := url.Values{}
	if agent, _ := cmd.Flags().GetString("agent"); agent != "" {
		agentID, err := resolveAgent(ctx, client, agent)
		if err != nil {
			return fmt.Errorf("resolve agent: %w", err)
		}
		params.Set("agent_id", agentID)
	}
	path := "/api/router/subscriptions"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	var result struct {
		Subscriptions []map[string]any `json:"subscriptions"`
	}
	if err := client.GetJSON(ctx, path, &result); err != nil {
		return fmt.Errorf("list subscriptions: %w", err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, result.Subscriptions)
	}
	fullID, _ := cmd.Flags().GetBool("full-id")
	cli.PrintTable(os.Stdout, routerSubscriptionHeaders, routerSubscriptionRows(result.Subscriptions, fullID))
	return nil
}

var routerSubscriptionHeaders = []string{"ID", "AGENT", "SCOPE", "ENABLED", "EVENTS", "CAPTURE MISS", "CREATED"}

func routerSubscriptionRows(subs []map[string]any, fullID bool) [][]string {
	rows := make([][]string, 0, len(subs))
	for _, s := range subs {
		enabled := "yes"
		if b, _ := s["enabled"].(bool); !b {
			enabled = "paused"
		}
		created := strVal(s, "created_at")
		if len(created) >= 10 {
			created = created[:10]
		}
		rows = append(rows, []string{
			displayID(strVal(s, "id"), fullID),
			displayID(strVal(s, "agent_id"), fullID),
			strVal(s, "scope_type") + ":" + displayID(strVal(s, "scope_id"), fullID),
			enabled,
			routerCount(s, "event_count"),
			routerCount(s, "capture_miss"),
			created,
		})
	}
	return rows
}

// routerCount renders a count, including zero.
func routerCount(m map[string]any, key string) string {
	if f, ok := m[key].(float64); ok {
		return strconv.FormatInt(int64(f), 10)
	}
	return strVal(m, key)
}

func printRouterSubscription(cmd *cobra.Command, sub map[string]any) error {
	if output, _ := cmd.Flags().GetString("output"); output == "table" {
		cli.PrintTable(os.Stdout, routerSubscriptionHeaders, routerSubscriptionRows([]map[string]any{sub}, true))
		return nil
	}
	return cli.PrintJSON(os.Stdout, sub)
}

func runRouterGet(cmd *cobra.Command, args []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	id, err := resolveRouterSubscriptionID(ctx, client, args[0])
	if err != nil {
		return err
	}
	var sub map[string]any
	if err := client.GetJSON(ctx, routerSubscriptionPath(id), &sub); err != nil {
		return fmt.Errorf("get subscription: %w", err)
	}
	return printRouterSubscription(cmd, sub)
}

func runRouterAction(cmd *cobra.Command, input, action string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	id, err := resolveRouterSubscriptionID(ctx, client, input)
	if err != nil {
		return err
	}
	var sub map[string]any
	if err := client.PostJSON(ctx, routerSubscriptionPath(id)+"/"+action, map[string]any{}, &sub); err != nil {
		return fmt.Errorf("%s subscription: %w", action, err)
	}
	return printRouterSubscription(cmd, sub)
}

func runRouterDelete(cmd *cobra.Command, args []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	id, err := resolveRouterSubscriptionID(ctx, client, args[0])
	if err != nil {
		return err
	}
	if err := client.DeleteJSON(ctx, routerSubscriptionPath(id)); err != nil {
		return fmt.Errorf("delete subscription: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Deleted subscription %s\n", id)
	return nil
}

// parseRouterSince accepts an RFC 3339 time or a duration back from now.
func parseRouterSince(v string, now time.Time) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t.UTC().Format(time.RFC3339Nano), nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return "", fmt.Errorf("--since must be an RFC 3339 time or a positive duration such as 2h")
	}
	return now.Add(-d).UTC().Format(time.RFC3339Nano), nil
}

func runRouterEvents(cmd *cobra.Command, _ []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	params := url.Values{}
	if sub, _ := cmd.Flags().GetString("subscription"); sub != "" {
		id, err := resolveRouterSubscriptionID(ctx, client, sub)
		if err != nil {
			return err
		}
		params.Set("subscription_id", id)
	}
	if agent, _ := cmd.Flags().GetString("agent"); agent != "" {
		agentID, err := resolveAgent(ctx, client, agent)
		if err != nil {
			return fmt.Errorf("resolve agent: %w", err)
		}
		params.Set("agent_id", agentID)
	}
	since, _ := cmd.Flags().GetString("since")
	sinceParam, err := parseRouterSince(since, time.Now())
	if err != nil {
		return err
	}
	if sinceParam != "" {
		params.Set("since", sinceParam)
	}
	for _, name := range []string{"state", "source"} {
		if v, _ := cmd.Flags().GetString(name); v != "" {
			params.Set(name, v)
		}
	}
	if limit, _ := cmd.Flags().GetInt("limit"); limit > 0 {
		params.Set("limit", strconv.Itoa(limit))
	}
	path := "/api/router/events"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	var result struct {
		Events []map[string]any `json:"events"`
	}
	if err := client.GetJSON(ctx, path, &result); err != nil {
		return fmt.Errorf("list events: %w", err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, result.Events)
	}
	rows := make([][]string, 0, len(result.Events))
	for _, e := range result.Events {
		self := ""
		if b, _ := e["self"].(bool); b {
			self = "self"
		}
		rows = append(rows, []string{
			strVal(e, "created_at"),
			strVal(e, "kind"),
			strVal(e, "issue_identifier"),
			strVal(e, "actor_type"),
			strVal(e, "source"),
			self,
			displayID(strVal(e, "subscription_id"), false),
		})
	}
	cli.PrintTable(os.Stdout, []string{"CAPTURED", "KIND", "ISSUE", "ACTOR", "SOURCE", "SELF", "SUBSCRIPTION"}, rows)
	return nil
}

func runRouterScope(cmd *cobra.Command, args []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	id, err := resolveRouterSubscriptionID(ctx, client, args[0])
	if err != nil {
		return err
	}
	path := routerSubscriptionPath(id) + "/scope"
	if all, _ := cmd.Flags().GetBool("all"); all {
		path += "?include_closed=true"
	}
	var result struct {
		Issues []map[string]any `json:"issues"`
	}
	if err := client.GetJSON(ctx, path, &result); err != nil {
		return fmt.Errorf("get scope: %w", err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, result.Issues)
	}
	rows := make([][]string, 0, len(result.Issues))
	for _, i := range result.Issues {
		rows = append(rows, []string{
			strVal(i, "identifier"),
			strVal(i, "status"),
			strVal(i, "assignee_type"),
			strVal(i, "updated_at"),
			strVal(i, "title"),
		})
	}
	cli.PrintTable(os.Stdout, []string{"ISSUE", "STATUS", "ASSIGNEE", "UPDATED", "TITLE"}, rows)
	return nil
}
