// Modified for the mouse9527/multica fork: Router inbox S3b (M-133).

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

// router inbox-rules shows and sets the rule list that sorts the owner's
// inbox items in shadow mode, and reports how often the owner still acted on
// an item the rules would have handled. It answers 404 unless
// router_dispatch is on.
var routerInboxRulesCmd = &cobra.Command{
	Use:   "inbox-rules",
	Short: "Show the workspace's inbox rules (shadow mode: verdicts are recorded, nothing is held)",
	Args:  exactArgs(0),
	RunE:  runRouterInboxRules,
}

var routerInboxRulesSetCmd = &cobra.Command{
	Use:   "set",
	Short: "Set the workspace's inbox rules from a file or the defaults (owners and admins)",
	Args:  exactArgs(0),
	RunE:  runRouterInboxRulesSet,
}

var routerInboxRulesReportCmd = &cobra.Command{
	Use:   "report",
	Short: "Per rule, how many classified items the owner acted on afterwards (mis-hold rate of handle rules)",
	Args:  exactArgs(0),
	RunE:  runRouterInboxRulesReport,
}

func init() {
	routerCmd.AddCommand(routerInboxRulesCmd)
	routerInboxRulesCmd.AddCommand(routerInboxRulesSetCmd, routerInboxRulesReportCmd)
	routerInboxRulesCmd.Flags().String("output", "table", "Output format: table or json")
	routerInboxRulesSetCmd.Flags().String("file", "", `JSON file with the rules: {"rules": [{"name", "types", "severities", "to_statuses", "verdict"}]}`)
	routerInboxRulesSetCmd.Flags().Bool("defaults", false, "Store the default rule list (needs --waiting-status)")
	routerInboxRulesSetCmd.Flags().StringSlice("waiting-status", nil, "With --defaults: status key that waits for the owner (repeatable), e.g. started_2")
	routerInboxRulesSetCmd.Flags().Bool("off", false, "Remove the rule list: stop classifying this workspace's items")
	routerInboxRulesSetCmd.Flags().String("output", "table", "Output format: table or json")
	routerInboxRulesReportCmd.Flags().String("since", "", "How far back to look, as a duration (default 168h, at most 720h)")
	routerInboxRulesReportCmd.Flags().String("window", "", "How long after an item an owner action counts, as a duration (default 24h, 1h-168h)")
	routerInboxRulesReportCmd.Flags().String("output", "table", "Output format: table or json")
}

func runRouterInboxRules(cmd *cobra.Command, _ []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var out map[string]any
	if err := client.GetJSON(ctx, "/api/router/inbox-rules", &out); err != nil {
		return fmt.Errorf("get inbox rules: %w", err)
	}
	return printInboxRules(cmd, out)
}

func runRouterInboxRulesSet(cmd *cobra.Command, _ []string) error {
	off, _ := cmd.Flags().GetBool("off")
	defaults, _ := cmd.Flags().GetBool("defaults")
	file, _ := cmd.Flags().GetString("file")
	statuses, _ := cmd.Flags().GetStringSlice("waiting-status")
	chosen := 0
	for _, on := range []bool{off, defaults, file != ""} {
		if on {
			chosen++
		}
	}
	if chosen != 1 {
		return fmt.Errorf("give exactly one of --file, --defaults or --off")
	}
	if defaults && len(statuses) == 0 {
		return fmt.Errorf("--defaults needs at least one --waiting-status")
	}
	body := map[string]any{"off": off}
	switch {
	case defaults:
		body["defaults"], body["waiting_statuses"] = true, statuses
	case file != "":
		raw, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		var in struct {
			Rules json.RawMessage `json:"rules"`
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil || len(in.Rules) == 0 {
			return fmt.Errorf(`%s: want {"rules": [...]}`, file)
		}
		body["rules"] = in.Rules
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var out map[string]any
	if err := client.PutJSON(ctx, "/api/router/inbox-rules", body, &out); err != nil {
		return fmt.Errorf("set inbox rules: %w", err)
	}
	return printInboxRules(cmd, out)
}

func printInboxRules(cmd *cobra.Command, out map[string]any) error {
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, out)
	}
	enabled, _ := out["enabled"].(bool)
	fmt.Fprintf(os.Stdout, "Classifying in the answering server: %v\n", enabled)
	cfg, _ := out["config"].(map[string]any)
	rules, _ := cfg["rules"].([]any)
	if len(rules) == 0 {
		fmt.Fprintln(os.Stdout, "Workspace rules: none (items are not classified in this workspace)")
	} else {
		fmt.Fprintln(os.Stdout, "Workspace rules (first match decides; gray types are always gray):")
		table := make([][]string, 0, len(rules))
		for i, x := range rules {
			r, _ := x.(map[string]any)
			table = append(table, []string{
				fmt.Sprint(i + 1), strVal(r, "name"), strVal(r, "verdict"),
				anyOrAll(r["types"]), anyOrAll(r["severities"]), anyOrAll(r["to_statuses"]),
			})
		}
		cli.PrintTable(os.Stdout, []string{"#", "RULE", "VERDICT", "TYPES", "SEVERITIES", "TO STATUSES"}, table)
	}
	if stats, _ := out["stats"].(map[string]any); stats != nil {
		fmt.Fprintf(os.Stdout, "This server since it started: verdicts %s, failures %s\n",
			countsVal(stats["verdicts"]), countVal(stats["failures"]))
	}
	return nil
}

func anyOrAll(v any) string {
	if list, _ := v.([]any); len(list) == 0 {
		return "(any)"
	}
	return joinAny(v)
}

func runRouterInboxRulesReport(cmd *cobra.Command, _ []string) error {
	params := url.Values{}
	for _, name := range []string{"since", "window"} {
		if v, _ := cmd.Flags().GetString(name); v != "" {
			params.Set(name, v)
		}
	}
	path := "/api/router/inbox-rules/report"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	var out map[string]any
	if err := client.GetJSON(ctx, path, &out); err != nil {
		return fmt.Errorf("inbox rules report: %w", err)
	}
	if output, _ := cmd.Flags().GetString("output"); output == "json" {
		return cli.PrintJSON(os.Stdout, out)
	}
	fmt.Fprintf(os.Stdout, "Items since %s, window %s.\n", strVal(out, "since"), strVal(out, "window"))
	fmt.Fprintf(os.Stdout, "Acted: %s\n\n", strVal(out, "definition"))
	headers := []string{"VERDICT", "RULE", "SAMPLES", "ACTED", "RATE", "PENDING"}
	cli.PrintTable(os.Stdout, headers, ruleReportRows(out["rules"], false))
	fmt.Fprintln(os.Stdout)
	fmt.Fprintln(os.Stdout, "By verdict (for handle, RATE is the mis-hold rate):")
	cli.PrintTable(os.Stdout, headers, ruleReportRows(out["verdicts"], true))
	fmt.Fprintf(os.Stdout, "\nNot classified: %s; classified but never reached the inbox: %s\n",
		countVal(out["unclassified"]), countVal(out["not_delivered"]))
	return nil
}

func ruleReportRows(v any, totals bool) [][]string {
	list, _ := v.([]any)
	rows := make([][]string, 0, len(list))
	for _, x := range list {
		r, _ := x.(map[string]any)
		rule := strVal(r, "rule")
		if totals {
			rule = "(all)"
		}
		rate := "-"
		if f, ok := r["rate"].(float64); ok {
			rate = fmt.Sprintf("%.1f%%", f*100)
		}
		rows = append(rows, []string{
			strVal(r, "verdict"), rule, countVal(r["samples"]), countVal(r["acted"]), rate, countVal(r["pending"]),
		})
	}
	return rows
}
