// Added by the mouse9527/multica fork: Evolution P2, the signal channel (M-136).

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

// Evolution commands. The server answers 404 while evolution_v1 is off, and
// signals answers only the Evolution agent's scheduled analysis run.

var evolutionCmd = &cobra.Command{
	Use:   "evolution",
	Short: "Evolution, the built-in agent that proposes improvements to agents, workflows, and the team",
}

var evolutionSignalsCmd = &cobra.Command{
	Use:   "signals",
	Short: "Read the next page of comments or chat messages for the current Evolution analysis run",
	Long: `Read the next page of comments or chat messages for the current Evolution analysis run.

Only the Evolution agent can call this, from the task of a scheduled analysis
issue. Each call continues where the previous one stopped; pass --after with
the previous page's next_after to re-read from there. Content is redacted;
private projects, private agents, and excluded projects and agents are never
returned.`,
	Example: `  $ multica evolution signals --source comment
  $ multica evolution signals --source chat --after <next_after>`,
	Args: exactArgs(0),
	RunE: runEvolutionSignals,
}

func init() {
	evolutionCmd.GroupID = groupCore
	rootCmd.AddCommand(evolutionCmd)
	evolutionCmd.AddCommand(evolutionSignalsCmd)
	evolutionSignalsCmd.Flags().String("source", "", "Source to read: comment or chat (required)")
	evolutionSignalsCmd.Flags().String("after", "", "Continue after this page token (next_after of a previous page)")
	evolutionSignalsCmd.Flags().Int("limit", 0, "Items per page, 1-200 (default 100)")
	evolutionSignalsCmd.Flags().String("output", "json", "Output format: json")
}

func runEvolutionSignals(cmd *cobra.Command, _ []string) error {
	source, _ := cmd.Flags().GetString("source")
	if source != "comment" && source != "chat" {
		return fmt.Errorf("--source must be comment or chat")
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
	var page map[string]any
	if err := client.GetJSON(ctx, "/api/evolution/signals?"+params.Encode(), &page); err != nil {
		return fmt.Errorf("read signals: %w", err)
	}
	return cli.PrintJSON(os.Stdout, page)
}
