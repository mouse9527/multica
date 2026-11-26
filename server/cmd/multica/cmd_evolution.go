// Added by the mouse9527/multica fork: Evolution P2, the signal channel (M-136);
// P3 proposals (M-137); P6 condense and team proposals (M-140); the metrics
// source (M-296).

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

// Evolution commands. The server answers 404 while evolution_v1 is off, and
// signals answers only the Evolution agent's scheduled analysis run.

var evolutionCmd = &cobra.Command{
	Use:   "evolution",
	Short: "Evolution, the built-in agent that proposes improvements to agents, workflows, and the team",
}

var evolutionSignalsCmd = &cobra.Command{
	Use:   "signals",
	Short: "Read comments, chat messages, or metrics for the current Evolution analysis run",
	Long: `Read comments, chat messages, or metrics for the current Evolution analysis run.

Only the Evolution agent can call this, from the task of a scheduled analysis
issue. For comment and chat, each call returns the next page and continues
where the previous one stopped; pass --after with the previous page's
next_after to re-read from there. Content is redacted; private projects,
private agents, and excluded projects and agents are never returned.

metrics returns one summary, numbers only, of the --days (default 14) before
the run's window end: per workflow step the entries, exits, dwell, and
returns; per agent the runs, failure and cancel rates, run time, short runs,
triggers, and handoffs; and the issues returned 3 or more times. It is served
only while the workspace turns the metrics source on.`,
	Example: `  $ multica evolution signals --source comment
  $ multica evolution signals --source chat --after <next_after>
  $ multica evolution signals --source metrics --days 14`,
	Args: exactArgs(0),
	RunE: runEvolutionSignals,
}

var evolutionProposeCmd = &cobra.Command{
	Use:   "propose",
	Short: "File an improvement proposal from the current Evolution analysis run or review",
	Long: `File an improvement proposal from the current Evolution analysis run or review.

Only the Evolution agent can call this, from the task of a scheduled analysis
issue or monthly condense review issue. The proposal is a JSON object (see the
multica-evolution skill):

  {
    "target": {"type": "agent|workflow|squad|team", "id": "<uuid>"},
    "field": "instructions",
    "topic": "a few words naming the problem",
    "title": "one line",
    "problem": "problem and cause; facts cite evidence, inferences are marked",
    "change": {"before": "current text, copied exactly", "after": "proposed text"},
    "evidence": [{"type": "comment|chat|issue", "id": "<uuid>", "note": "summary", "quote": "comments only, <= 120 characters"}],
    "expected_effect": "which signal moves, which way, over how long",
    "risk": "low|medium|high",
    "risk_reason": "why",
    "rollback": "how to undo"
  }

The server checks the rules and refuses with a reason (code and error) when one
is broken. The same proposal already open gets the new evidence as a comment
instead of a new issue ("outcome": "merged" or "duplicate"). Nothing is applied.

A condense review files shorter agent instructions with an empty evidence list;
the server attaches its measurement. A team proposal (target id: the
workspace's id; field new_agent, merge_agents, or routing) leaves change.before
empty and is carried out by hand.`,
	Example: `  $ multica evolution propose --file proposal.json
  $ multica evolution propose --file - < proposal.json`,
	Args: exactArgs(0),
	RunE: runEvolutionPropose,
}

func init() {
	evolutionCmd.GroupID = groupCore
	rootCmd.AddCommand(evolutionCmd)
	evolutionCmd.AddCommand(evolutionSignalsCmd)
	evolutionCmd.AddCommand(evolutionProposeCmd)
	evolutionProposeCmd.Flags().String("file", "", "Path to the proposal JSON, or - for stdin (required)")
	evolutionProposeCmd.Flags().String("output", "json", "Output format: json")
	evolutionSignalsCmd.Flags().String("source", "", "Source to read: comment, chat, or metrics (required)")
	evolutionSignalsCmd.Flags().String("after", "", "Continue after this page token (next_after of a previous page)")
	evolutionSignalsCmd.Flags().Int("limit", 0, "Items per page, 1-200 (default 100)")
	evolutionSignalsCmd.Flags().Int("days", 0, "metrics only: days before the run's window end, 1-30 (default 14)")
	evolutionSignalsCmd.Flags().String("output", "json", "Output format: json")
}

func runEvolutionSignals(cmd *cobra.Command, _ []string) error {
	source, _ := cmd.Flags().GetString("source")
	if source != "comment" && source != "chat" && source != "metrics" {
		return fmt.Errorf("--source must be comment, chat, or metrics")
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	params := url.Values{"source": {source}}
	if after, _ := cmd.Flags().GetString("after"); after != "" {
		params.Set("after", after)
	}
	if limit, _ := cmd.Flags().GetInt("limit"); limit > 0 {
		params.Set("limit", strconv.Itoa(limit))
	}
	if days, _ := cmd.Flags().GetInt("days"); days > 0 {
		params.Set("days", strconv.Itoa(days))
	}
	var page map[string]any
	if err := client.GetJSON(ctx, "/api/evolution/signals?"+params.Encode(), &page); err != nil {
		return fmt.Errorf("read signals: %w", err)
	}
	return cli.PrintJSON(os.Stdout, page)
}

func runEvolutionPropose(cmd *cobra.Command, _ []string) error {
	path, _ := cmd.Flags().GetString("file")
	if path == "" {
		return fmt.Errorf("--file is required (a path, or - for stdin)")
	}
	var raw []byte
	var err error
	if path == "-" {
		raw, err = io.ReadAll(cmd.InOrStdin())
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return fmt.Errorf("read proposal: %w", err)
	}
	var body json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return fmt.Errorf("proposal is not valid JSON: %w", err)
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	var result map[string]any
	if err := client.PostJSON(ctx, "/api/evolution/proposals", body, &result); err != nil {
		// A refusal carries the server's code and reason in its body.
		return fmt.Errorf("file proposal: %w", err)
	}
	return cli.PrintJSON(os.Stdout, result)
}
