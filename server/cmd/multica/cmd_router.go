// Modified for the mouse9527/multica fork: Router MVP, slices S1 (M-85) and S2 (M-86), Router settings by instruction (M-105), and the Super Agent name in help (M-148).

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
//
// People and the UI call a Router a "Super Agent" (M-124); the command keeps
// its technical name. Help text says both, because an agent asked to "make
// this chat a Super Agent" searches `multica --help` for that phrase and has
// no other way to learn the command is called router.

var routerCmd = &cobra.Command{
	Use:   "router",
	Short: "Super Agent (technical name: router): a chat that runs on events from subscribed issue trees or projects",
	Example: `  # Make the current chat a Super Agent for an issue tree (inside a chat run)
  $ multica router subscribe --issue M-12 --anchor-chat current

  # Is this chat a Super Agent, and what does it cover?
  $ multica router get --chat current --output table

A Super Agent subscription is not an issue subscription: ` + "`multica issue subscriber add`" + `
only sends inbox notifications and never starts a run.`,
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
	Use:   "get [<subscription-id> | --chat <current|chat-id>]",
	Short: "Get a subscription, including its capture_miss count, or with --chat every subscription of a Super Agent",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runRouterGet,
}

var routerPauseCmd = &cobra.Command{
	Use:   "pause [<subscription-id> | --chat <current|chat-id>]",
	Short: "Stop capturing events for a subscription, or with --chat for a whole Super Agent",
	Args:  cobra.MaximumNArgs(1),
	RunE:  func(cmd *cobra.Command, args []string) error { return runRouterAction(cmd, args, "pause") },
}

var routerResumeCmd = &cobra.Command{
	Use:   "resume [<subscription-id> | --chat <current|chat-id>]",
	Short: "Resume a subscription, or with --chat a whole Super Agent, starting from now",
	Args:  cobra.MaximumNArgs(1),
	RunE:  func(cmd *cobra.Command, args []string) error { return runRouterAction(cmd, args, "resume") },
}

var routerDeleteCmd = &cobra.Command{
	Use:   "delete [<subscription-id> | --chat <current|chat-id>]",
	Short: "Delete a subscription and its captured events, or with --chat every subscription of a Super Agent",
	Args:  cobra.MaximumNArgs(1),
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
	// Without a group the root help template leaves the command out entirely.
	routerCmd.GroupID = groupCore
	rootCmd.AddCommand(routerCmd)
	// The upstream issue subscriber commands are where an agent lands when it
	// takes "subscribe as a Super Agent" literally; point it at router instead.
	issueSubscriberCmd.Short = "Work with issue subscribers (inbox notifications only; a Super Agent subscription is `multica router subscribe`)"
	issueSubscriberAddCmd.Long = issueSubscriberAddCmd.Short + ".\n\n" +
		"This only sends the subscriber inbox notifications about the issue; it never starts a run.\n" +
		"To have a chat run on an issue's events (a Super Agent), use `multica router subscribe --issue <id> --anchor-chat current`."
	routerCmd.AddCommand(routerSubscribeCmd, routerListCmd, routerGetCmd, routerPauseCmd,
		routerResumeCmd, routerDeleteCmd, routerEventsCmd, routerScopeCmd)

	routerSubscribeCmd.Flags().String("agent", "", "Agent name or ID (default: the calling agent)")
	routerSubscribeCmd.Flags().String("issue", "", "Root issue of the subscribed tree (key or UUID)")
	routerSubscribeCmd.Flags().String("project", "", "Subscribed project (ID)")
	routerSubscribeCmd.Flags().StringSlice("events", nil, "Event types to capture (default: all): issue_created, status_changed, assignee_changed, comment_created, task_failed")
	routerSubscribeCmd.Flags().String("anchor-chat", "", "Bind the subscription to a chat (a Super Agent): current (inside a chat run) or a chat session UUID; a --project scope without --anchor gets a board issue (needs router_chat)")
	routerSubscribeCmd.Flags().String("output", "json", "Output format: table or json")
	for _, c := range []*cobra.Command{routerGetCmd, routerPauseCmd, routerResumeCmd, routerDeleteCmd} {
		c.Flags().String("chat", "", "Act on every subscription of the Super Agent bound to this chat: current (inside a chat run) or a chat session UUID")
	}

	routerListCmd.Flags().String("agent", "", "Only this agent's subscriptions (name or ID)")
	routerListCmd.Flags().String("output", "table", "Output format: table or json")
	routerListCmd.Flags().Bool("full-id", false, "Show full UUIDs in table output")

	routerGetCmd.Flags().String("output", "json", "Output format: table or json")
	routerPauseCmd.Flags().String("output", "json", "Output format: table or json")
	routerResumeCmd.Flags().String("output", "json", "Output format: table or json")
	routerDeleteCmd.Flags().String("output", "json", "Output format: table or json")

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
	if err := routerBatchingBody(ctx, cmd, client, body); err != nil {
		return err
	}
	board := ""
	if chatRef, _ := cmd.Flags().GetString("anchor-chat"); chatRef != "" {
		chat, err := resolveRouterChat(ctx, client, chatRef)
		if err != nil {
			return err
		}
		body["anchor_chat_session_id"] = chat.ID
		if project != "" && body["anchor_issue_id"] == nil {
			if board, err = routerChatBoard(ctx, client, body["scope_id"].(string), chat); err != nil {
				return err
			}
			body["anchor_issue_id"] = board
		}
	}

	var sub map[string]any
	if err := client.PostJSON(ctx, "/api/router/subscriptions", body, &sub); err != nil {
		if board != "" {
			_ = client.DeleteJSON(ctx, "/api/issues/"+url.PathEscape(board))
		}
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

// RECONCILED THROUGH bounds what CAPTURE MISS covers: changes after it have
// not been checked by a completed reconcile pass yet.
var routerSubscriptionHeaders = []string{"ID", "AGENT", "SCOPE", "ENABLED", "EVENTS", "CAPTURE MISS", "RECONCILED THROUGH", "CREATED"}

func routerSubscriptionRows(subs []map[string]any, fullID bool) [][]string {
	rows := make([][]string, 0, len(subs))
	for _, s := range subs {
		enabled := "yes"
		if b, _ := s["enabled"].(bool); !b {
			enabled = "paused"
		} else if reason := strVal(s, "paused_reason"); reason != "" {
			// Capture goes on; only dispatch stopped.
			enabled = "dispatch paused (" + reason + ")"
		}
		created := strVal(s, "created_at")
		if len(created) >= 10 {
			created = created[:10]
		}
		reconciled := strVal(s, "reconciled_through")
		if reconciled == "" {
			reconciled = "never"
		}
		rows = append(rows, []string{
			displayID(strVal(s, "id"), fullID),
			displayID(strVal(s, "agent_id"), fullID),
			strVal(s, "scope_type") + ":" + displayID(strVal(s, "scope_id"), fullID),
			enabled,
			routerCount(s, "event_count"),
			routerCount(s, "capture_miss"),
			reconciled,
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
	if chatRef, _ := cmd.Flags().GetString("chat"); chatRef != "" {
		if len(args) > 0 {
			return fmt.Errorf("pass a subscription id or --chat, not both")
		}
		chat, err := resolveRouterChat(ctx, client, chatRef)
		if err != nil {
			return err
		}
		subs, err := routerChatSubscriptions(ctx, client, chat.ID)
		if err != nil {
			return err
		}
		if output, _ := cmd.Flags().GetString("output"); output == "table" {
			if len(subs) == 0 {
				fmt.Fprintf(os.Stderr, "Chat %s is not a Router.\n", chat.ID)
			}
			cli.PrintTable(os.Stdout, routerSubscriptionHeaders, routerSubscriptionRows(subs, true))
			return nil
		}
		return cli.PrintJSON(os.Stdout, map[string]any{"chat_session_id": chat.ID, "subscriptions": subs})
	}
	if len(args) != 1 {
		return fmt.Errorf("pass a subscription id, or --chat for a Router")
	}
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

func runRouterAction(cmd *cobra.Command, args []string, action string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	ids, err := routerTargets(ctx, cmd, client, args)
	if err != nil {
		return err
	}
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		var sub map[string]any
		if err := client.PostJSON(ctx, routerSubscriptionPath(id)+"/"+action, map[string]any{}, &sub); err != nil {
			return fmt.Errorf("%s subscription %s: %w", action, id, err)
		}
		out = append(out, sub)
	}
	return printRouterSubscriptions(cmd, out)
}

func runRouterDelete(cmd *cobra.Command, args []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	ids, err := routerTargets(ctx, cmd, client, args)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := client.DeleteJSON(ctx, routerSubscriptionPath(id)); err != nil {
			return fmt.Errorf("delete subscription %s: %w", id, err)
		}
		fmt.Fprintf(os.Stderr, "Deleted subscription %s\n", id)
	}
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
