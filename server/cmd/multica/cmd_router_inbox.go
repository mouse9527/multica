// Modified for the mouse9527/multica fork: Router inbox S3a (M-118).

package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

// router inbox-resolve shows how the owner inbox stands against the issues
// that wait for the owner, and sets which owner and statuses that means.
var routerInboxResolveCmd = &cobra.Command{
	Use:   "inbox-resolve",
	Short: "Show the owner inbox against the issues that wait for the owner (excess, missing, archive counts)",
	Args:  exactArgs(0),
	RunE:  runRouterInboxResolve,
}

var routerInboxResolveSetCmd = &cobra.Command{
	Use:   "set",
	Short: "Set the workspace's inbox owner and waiting statuses (owners and admins)",
	Args:  exactArgs(0),
	RunE:  runRouterInboxResolveSet,
}

func init() {
	routerCmd.AddCommand(routerInboxResolveCmd)
	routerInboxResolveCmd.AddCommand(routerInboxResolveSetCmd)
	routerInboxResolveCmd.Flags().String("output", "table", "Output format: table or json")
	routerInboxResolveSetCmd.Flags().String("owner", "", "Owner user ID (default: you)")
	routerInboxResolveSetCmd.Flags().StringSlice("waiting-status", nil, "Status key that waits for the owner (repeatable), e.g. started_2")
	routerInboxResolveSetCmd.Flags().Bool("off", false, "Remove the setting: stop resolving this workspace's inbox")
	routerInboxResolveSetCmd.Flags().String("output", "table", "Output format: table or json")
}

func runRouterInboxResolve(cmd *cobra.Command, _ []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var out map[string]any
	if err := client.GetJSON(ctx, "/api/router/inbox-resolve", &out); err != nil {
		return fmt.Errorf("get inbox resolve: %w", err)
	}
	return printInboxResolve(cmd, out)
}

func runRouterInboxResolveSet(cmd *cobra.Command, _ []string) error {
	off, _ := cmd.Flags().GetBool("off")
	owner, _ := cmd.Flags().GetString("owner")
	statuses, _ := cmd.Flags().GetStringSlice("waiting-status")
	if !off && len(statuses) == 0 {
		return fmt.Errorf("give at least one --waiting-status, or --off")
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	body := map[string]any{"off": off}
	if !off {
		body["owner_user_id"] = owner
		body["waiting_statuses"] = statuses
	}
	var out map[string]any
	if err := client.PutJSON(ctx, "/api/router/inbox-resolve", body, &out); err != nil {
		return fmt.Errorf("set inbox resolve: %w", err)
	}
	return printInboxResolve(cmd, out)
}

func printInboxResolve(cmd *cobra.Command, out map[string]any) error {
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, out)
	}
	enabled, _ := out["enabled"].(bool)
	fmt.Fprintf(os.Stdout, "Enabled in the answering server: %v\n", enabled)
	cfg, _ := out["config"].(map[string]any)
	if cfg == nil {
		fmt.Fprintln(os.Stdout, "Workspace setting: none (inbox resolve is off for this workspace)")
	} else {
		fmt.Fprintf(os.Stdout, "Owner: %s\nWaiting statuses: %s\n", strVal(cfg, "owner_user_id"), joinAny(cfg["waiting_statuses"]))
	}
	if survey, _ := out["survey"].(map[string]any); survey != nil {
		fmt.Fprintf(os.Stdout, "Now: excess %s, outside the wait %s, missing %s\n",
			countVal(survey["excess"]), countVal(survey["outside_wait"]), countsVal(survey["missing"]))
	}
	if stats, _ := out["stats"].(map[string]any); stats != nil {
		fmt.Fprintf(os.Stdout, "This server since %s: archived %s, leaves %s, still waiting %s, failures %s, reconcile passes %s\n",
			strVal(stats, "since"), countsVal(stats["archived"]), countVal(stats["leaves"]),
			countVal(stats["still_waiting"]), countVal(stats["failures"]), countVal(stats["reconcile_passes"]))
		if last, _ := stats["last_reconcile"].(map[string]any); last != nil {
			fmt.Fprintf(os.Stdout, "Last reconcile here at %s: archived %s on %s issues, missing %s, outside the wait %s\n",
				strVal(last, "at"), countVal(last["archived"]), countVal(last["archived_issues"]),
				countsVal(last["missing"]), countVal(last["outside_wait"]))
		}
	}
	return nil
}

func countVal(v any) string {
	if f, ok := v.(float64); ok {
		return fmt.Sprintf("%d", int64(f))
	}
	return "0"
}

// countsVal prints a {reason: count} map as "a=1 b=2", sorted, or "0".
func countsVal(v any) string {
	m, _ := v.(map[string]any)
	if len(m) == 0 {
		return "0"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+countVal(m[k]))
	}
	return strings.Join(parts, " ")
}

func joinAny(v any) string {
	list, _ := v.([]any)
	parts := make([]string, 0, len(list))
	for _, x := range list {
		parts = append(parts, fmt.Sprint(x))
	}
	if len(parts) == 0 {
		return "(none)"
	}
	return strings.Join(parts, ", ")
}
