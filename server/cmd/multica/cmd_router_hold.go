// Modified for the mouse9527/multica fork: Router v6, slice 2 (M-97), and Router inbox S3b (M-133).

package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

// router holds lists what the Router hold recorded: in shadow mode, the inbox
// items it would have taken. It answers 404 unless router_dispatch is on.
var routerHoldsCmd = &cobra.Command{
	Use:   "holds",
	Short: "List the inbox items the Router hold took or, in shadow mode, would have taken",
	Args:  exactArgs(0),
	RunE:  runRouterHolds,
}

func init() {
	routerCmd.AddCommand(routerHoldsCmd)
	routerHoldsCmd.Flags().String("subscription", "", "Only this subscription's items (ID or prefix)")
	routerHoldsCmd.Flags().Int("limit", 0, "Maximum items to return (1-200, default 50)")
	routerHoldsCmd.Flags().Bool("stats", false, "Print the interceptor stats of the server process that answered instead of the items")
	routerHoldsCmd.Flags().String("output", "table", "Output format: table or json")
}

var holdHeaders = []string{"ID", "CREATED", "STATUS", "ISSUE", "TYPE", "IN INBOX", "DECIDE µs", "VERDICT", "TITLE"}

func holdRows(holds []map[string]any) [][]string {
	rows := make([][]string, 0, len(holds))
	for _, x := range holds {
		inInbox := "no"
		if v, _ := x["in_inbox"].(bool); v {
			inInbox = "yes"
		}
		decide := ""
		if v, ok := x["decide_micros"].(float64); ok {
			decide = strconv.FormatInt(int64(v), 10)
		}
		rows = append(rows, []string{
			displayID(strVal(x, "id"), false),
			strVal(x, "created_at"),
			strVal(x, "status"),
			strVal(x, "issue_identifier"),
			strVal(x, "item_type"),
			inInbox,
			decide,
			holdVerdict(x),
			strVal(x, "title"),
		})
	}
	return rows
}

// holdVerdict is the rule verdict as "verdict (rule)", empty when the item
// was not classified.
func holdVerdict(x map[string]any) string {
	if v := strVal(x, "verdict"); v != "" {
		return v + " (" + strVal(x, "rule") + ")"
	}
	return ""
}

func runRouterHolds(cmd *cobra.Command, _ []string) error {
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
	path := "/api/router/holds"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	var result struct {
		Holds []map[string]any `json:"holds"`
		Stats map[string]any   `json:"stats"`
	}
	if err := client.GetJSON(ctx, path, &result); err != nil {
		return fmt.Errorf("list holds: %w", err)
	}
	if stats, _ := cmd.Flags().GetBool("stats"); stats {
		if result.Stats == nil {
			return fmt.Errorf("the hold is off in the server process that answered")
		}
		return cli.PrintJSON(os.Stdout, result.Stats)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, result.Holds)
	}
	cli.PrintTable(os.Stdout, holdHeaders, holdRows(result.Holds))
	return nil
}
