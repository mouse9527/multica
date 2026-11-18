// Modified for the mouse9527/multica fork: Router MVP, slice S2 (M-86), Router v6 slice 1 (M-96), Router settings by instruction (M-105), no rate pause (M-111), and the Super Agent name in help (M-148).

package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

// Router dispatch commands: subscription settings, state, anchor runs, the
// audit trail, and notify owner. State and notices answer 404 while
// router_dispatch is off.

var routerUpdateCmd = &cobra.Command{
	Use:   "update [<subscription-id> | --chat <current|chat-id>]",
	Short: "Change a subscription's anchor issue or chat, event types or batching; with --chat, every subscription of a Super Agent",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runRouterUpdate,
}

var routerStateCmd = &cobra.Command{
	Use:   "state",
	Short: "Read or write a subscription's state document",
}

var routerStateGetCmd = &cobra.Command{
	Use:   "get <subscription-id>",
	Short: "Print a subscription's state and revision",
	Args:  exactArgs(1),
	RunE:  runRouterStateGet,
}

var routerStateSetCmd = &cobra.Command{
	Use:   "set <subscription-id>",
	Short: "Replace a subscription's state (requires --expect-revision)",
	Args:  exactArgs(1),
	RunE:  runRouterStateSet,
}

var routerRunsCmd = &cobra.Command{
	Use:   "runs",
	Short: "List anchor runs started for subscriptions, newest first",
	Args:  exactArgs(0),
	RunE:  runRouterRuns,
}

var routerChangesCmd = &cobra.Command{
	Use:   "changes <subscription-id>",
	Short: "List who changed a subscription, and on whose authority",
	Args:  exactArgs(1),
	RunE:  runRouterChanges,
}

var notifyCmd = &cobra.Command{
	Use:   "notify",
	Short: "Send notices to people",
}

var notifyOwnerCmd = &cobra.Command{
	Use:   "owner",
	Short: "Notify the owner: of --subscription, else of the calling agent (a member notifies themself)",
	Args:  exactArgs(0),
	RunE:  runNotifyOwner,
}

var notifyListCmd = &cobra.Command{
	Use:   "list",
	Short: "List notices you received (as a member) or sent (as an agent)",
	Args:  exactArgs(0),
	RunE:  runNotifyList,
}

func init() {
	routerCmd.AddCommand(routerUpdateCmd, routerStateCmd, routerRunsCmd, routerChangesCmd)
	routerStateCmd.AddCommand(routerStateGetCmd, routerStateSetCmd)
	rootCmd.AddCommand(notifyCmd)
	notifyCmd.AddCommand(notifyOwnerCmd, notifyListCmd)

	for _, c := range []*cobra.Command{routerSubscribeCmd, routerUpdateCmd} {
		c.Flags().String("anchor", "", "Issue anchor runs go to (key or UUID; default for --issue: the tree root)")
		c.Flags().Int("debounce", 0, "Seconds without a new event before a batch starts (default 60)")
		c.Flags().Int("max-wait", 0, "Most seconds the oldest event waits before a batch starts (default 600)")
		c.Flags().Int("max-runs-per-hour", 0, "Runs per hour above which the owner is alerted, at most once an hour; dispatch never pauses for it (default 20)")
	}
	routerUpdateCmd.Flags().StringSlice("events", nil, "Event types to capture from now on")
	routerUpdateCmd.Flags().String("anchor-chat", "", "Chat that receives the batches instead of anchor runs: current (inside a chat run) or a chat session UUID; \"none\" goes back to anchor runs (needs router_chat)")
	routerUpdateCmd.Flags().String("chat", "", "Change every subscription of the Super Agent bound to this chat: current (inside a chat run) or a chat session UUID")
	routerUpdateCmd.Flags().String("output", "json", "Output format: table or json")

	routerStateGetCmd.Flags().String("output", "json", "Output format: text (state only) or json")
	routerStateSetCmd.Flags().Int64("expect-revision", -1, "Revision the new state replaces (from state get); required")
	routerStateSetCmd.Flags().String("state", "", "New state (inline)")
	routerStateSetCmd.Flags().String("state-file", "", "Read the new state from a file")
	routerStateSetCmd.Flags().Bool("state-stdin", false, "Read the new state from stdin")
	routerStateSetCmd.Flags().Bool("clear", false, "Set an empty state")
	routerStateSetCmd.Flags().String("output", "json", "Output format: table or json")

	routerRunsCmd.Flags().String("subscription", "", "Only this subscription's runs (ID or prefix)")
	routerRunsCmd.Flags().Int("limit", 0, "Maximum runs to return (1-200, default 50)")
	routerRunsCmd.Flags().String("output", "table", "Output format: table or json")

	routerChangesCmd.Flags().String("output", "table", "Output format: table or json")

	notifyOwnerCmd.Flags().String("title", "", "One-line summary (required)")
	notifyOwnerCmd.Flags().String("body", "", "Details (inline)")
	notifyOwnerCmd.Flags().String("body-file", "", "Read details from a file")
	notifyOwnerCmd.Flags().Bool("body-stdin", false, "Read details from stdin")
	notifyOwnerCmd.Flags().StringArray("ref", nil, "Issue the notice is about (key or UUID; repeatable; the first opens from the inbox)")
	notifyOwnerCmd.Flags().String("urgency", "normal", "normal, or high when the owner must act")
	notifyOwnerCmd.Flags().String("dedupe-key", "", "Send at most once per key, sender and recipient")
	notifyOwnerCmd.Flags().String("subscription", "", "Send to this subscription's owner (ID or prefix)")
	notifyOwnerCmd.Flags().String("output", "json", "Output format: table or json")

	notifyListCmd.Flags().String("subscription", "", "Only notices from this subscription (ID or prefix)")
	notifyListCmd.Flags().Int("limit", 0, "Maximum notices to return (1-200, default 50)")
	notifyListCmd.Flags().String("output", "table", "Output format: table or json")
}

// routerBatchingBody adds the batching and anchor flags that were set.
func routerBatchingBody(ctx context.Context, cmd *cobra.Command, client *cli.APIClient, body map[string]any) error {
	if anchor, _ := cmd.Flags().GetString("anchor"); anchor != "" {
		ref, err := resolveIssueRef(ctx, client, anchor)
		if err != nil {
			return fmt.Errorf("resolve anchor: %w", err)
		}
		body["anchor_issue_id"] = ref.ID
	}
	for flag, field := range map[string]string{
		"debounce": "debounce_seconds", "max-wait": "max_wait_seconds", "max-runs-per-hour": "max_runs_per_hour",
	} {
		if v, _ := cmd.Flags().GetInt(flag); v != 0 {
			body[field] = v
		}
	}
	return nil
}

func runRouterUpdate(cmd *cobra.Command, args []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	body := map[string]any{}
	if err := routerBatchingBody(ctx, cmd, client, body); err != nil {
		return err
	}
	if cmd.Flags().Changed("events") {
		events, _ := cmd.Flags().GetStringSlice("events")
		body["event_types"] = events
	}
	if cmd.Flags().Changed("anchor-chat") {
		chat, _ := cmd.Flags().GetString("anchor-chat")
		chat = strings.TrimSpace(chat)
		if chat == "" {
			return fmt.Errorf("--anchor-chat needs current, a chat session id, or none")
		}
		if chat == currentChat {
			resolved, err := resolveRouterChat(ctx, client, chat)
			if err != nil {
				return err
			}
			chat = resolved.ID
		}
		body["anchor_chat_session_id"] = chat
	}
	if len(body) == 0 {
		return fmt.Errorf("nothing to change: pass --anchor, --anchor-chat, --events, --debounce, --max-wait or --max-runs-per-hour")
	}
	ids, err := routerTargets(ctx, cmd, client, args)
	if err != nil {
		return err
	}
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		var sub map[string]any
		if err := client.PatchJSON(ctx, routerSubscriptionPath(id), body, &sub); err != nil {
			return fmt.Errorf("update subscription %s: %w", id, err)
		}
		out = append(out, sub)
	}
	return printRouterSubscriptions(cmd, out)
}

func runRouterStateGet(cmd *cobra.Command, args []string) error {
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
	var st map[string]any
	if err := client.GetJSON(ctx, routerSubscriptionPath(id)+"/state", &st); err != nil {
		return fmt.Errorf("get state: %w", err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "text" {
		fmt.Fprintf(os.Stderr, "revision %s\n", routerCount(st, "revision"))
		fmt.Fprint(os.Stdout, strVal(st, "state"))
		return nil
	}
	return cli.PrintJSON(os.Stdout, st)
}

func runRouterStateSet(cmd *cobra.Command, args []string) error {
	revision, _ := cmd.Flags().GetInt64("expect-revision")
	if revision < 0 {
		return fmt.Errorf("--expect-revision is required: read it with `multica router state get`")
	}
	clear, _ := cmd.Flags().GetBool("clear")
	state, given, err := resolveTextFlag(cmd, "state")
	if err != nil {
		return err
	}
	if clear == given {
		return fmt.Errorf("pass exactly one of --state, --state-file, --state-stdin or --clear")
	}
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
	var st map[string]any
	if err := client.PutJSON(ctx, routerSubscriptionPath(id)+"/state", map[string]any{
		"state": state, "expected_revision": revision,
	}, &st); err != nil {
		return fmt.Errorf("set state: %w", err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "table" {
		cli.PrintTable(os.Stdout, []string{"SUBSCRIPTION", "REVISION", "UPDATED"},
			[][]string{{strVal(st, "subscription_id"), routerCount(st, "revision"), strVal(st, "updated_at")}})
		return nil
	}
	return cli.PrintJSON(os.Stdout, st)
}

func runRouterRuns(cmd *cobra.Command, _ []string) error {
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
	if limit, _ := cmd.Flags().GetInt("limit"); limit > 0 {
		params.Set("limit", strconv.Itoa(limit))
	}
	path := "/api/router/dispatches"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	var result struct {
		Dispatches []map[string]any `json:"dispatches"`
	}
	if err := client.GetJSON(ctx, path, &result); err != nil {
		return fmt.Errorf("list runs: %w", err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, result.Dispatches)
	}
	rows := make([][]string, 0, len(result.Dispatches))
	for _, d := range result.Dispatches {
		outcome := strVal(d, "outcome")
		if outcome == "" {
			outcome = "open"
		}
		rows = append(rows, []string{
			strVal(d, "created_at"),
			displayID(strVal(d, "subscription_id"), false),
			displayID(strVal(d, "task_id"), false),
			strVal(d, "task_status"),
			routerCount(d, "event_count"),
			outcome,
		})
	}
	cli.PrintTable(os.Stdout, []string{"STARTED", "SUBSCRIPTION", "TASK", "TASK STATUS", "EVENTS", "OUTCOME"}, rows)
	return nil
}

func runRouterChanges(cmd *cobra.Command, args []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	id, err := resolveRouterSubscriptionID(ctx, client, args[0])
	if err != nil {
		// A deleted subscription is no longer listed; take a full UUID as is.
		id = args[0]
	}
	var result struct {
		Changes []map[string]any `json:"changes"`
	}
	if err := client.GetJSON(ctx, routerSubscriptionPath(id)+"/changes", &result); err != nil {
		return fmt.Errorf("list changes: %w", err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, result.Changes)
	}
	rows := make([][]string, 0, len(result.Changes))
	for _, c := range result.Changes {
		rows = append(rows, []string{
			strVal(c, "created_at"),
			strVal(c, "action"),
			strVal(c, "actor_type") + " " + displayID(strVal(c, "actor_id"), false),
			displayID(strVal(c, "authorized_by_user_id"), false),
			displayID(strVal(c, "via_agent_id"), false),
		})
	}
	cli.PrintTable(os.Stdout, []string{"AT", "ACTION", "ACTOR", "AUTHORIZED BY", "VIA AGENT"}, rows)
	return nil
}

func runNotifyOwner(cmd *cobra.Command, _ []string) error {
	title, _ := cmd.Flags().GetString("title")
	if title == "" {
		return fmt.Errorf("--title is required")
	}
	body, _, err := resolveTextFlag(cmd, "body")
	if err != nil {
		return err
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	urgency, _ := cmd.Flags().GetString("urgency")
	dedupe, _ := cmd.Flags().GetString("dedupe-key")
	req := map[string]any{"title": title, "body": body, "urgency": urgency, "dedupe_key": dedupe}
	if sub, _ := cmd.Flags().GetString("subscription"); sub != "" {
		id, err := resolveRouterSubscriptionID(ctx, client, sub)
		if err != nil {
			return err
		}
		req["subscription_id"] = id
	}
	refs, _ := cmd.Flags().GetStringArray("ref")
	ids := make([]string, 0, len(refs))
	for _, r := range refs {
		ref, err := resolveIssueRef(ctx, client, r)
		if err != nil {
			return fmt.Errorf("resolve --ref %s: %w", r, err)
		}
		ids = append(ids, ref.ID)
	}
	req["refs"] = ids
	var result struct {
		Notice  map[string]any `json:"notice"`
		Created bool           `json:"created"`
	}
	if err := client.PostJSON(ctx, "/api/router/notices", req, &result); err != nil {
		return fmt.Errorf("notify owner: %w", err)
	}
	if !result.Created {
		fmt.Fprintf(os.Stderr, "A notice with dedupe key %q was already sent; nothing new was sent.\n", dedupe)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "table" {
		cli.PrintTable(os.Stdout, noticeHeaders, noticeRows([]map[string]any{result.Notice}))
		return nil
	}
	return cli.PrintJSON(os.Stdout, result.Notice)
}

var noticeHeaders = []string{"ID", "CREATED", "URGENCY", "TO", "INBOX", "TITLE"}

func noticeRows(notices []map[string]any) [][]string {
	rows := make([][]string, 0, len(notices))
	for _, n := range notices {
		inbox := ""
		if states, ok := n["channel_states"].(map[string]any); ok {
			if s, ok := states["inbox"].(map[string]any); ok {
				inbox = strVal(s, "state")
			}
		}
		rows = append(rows, []string{
			displayID(strVal(n, "id"), false),
			strVal(n, "created_at"),
			strVal(n, "urgency"),
			displayID(strVal(n, "recipient_user_id"), false),
			inbox,
			strVal(n, "title"),
		})
	}
	return rows
}

func runNotifyList(cmd *cobra.Command, _ []string) error {
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
	if limit, _ := cmd.Flags().GetInt("limit"); limit > 0 {
		params.Set("limit", strconv.Itoa(limit))
	}
	path := "/api/router/notices"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	var result struct {
		Notices []map[string]any `json:"notices"`
	}
	if err := client.GetJSON(ctx, path, &result); err != nil {
		return fmt.Errorf("list notices: %w", err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, result.Notices)
	}
	cli.PrintTable(os.Stdout, noticeHeaders, noticeRows(result.Notices))
	return nil
}
