// Added by the mouse9527/multica fork: importing GitHub issues on request.

package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

var issueImportCmd = &cobra.Command{
	Use:   "import <owner/repo#N | https://github.com/owner/repo/issues/N>",
	Short: "Import a GitHub issue",
	Long: `Import one GitHub issue, with its comments, into the workspace. From then on
it is followed like any GitHub issue: its edits and new comments on GitHub
reach Multica. GitHub issues are never imported on their own.

The repository must be registered on a project of the workspace, and the
number must be an issue, not a pull request. Importing an issue again returns
the issue the first import made and reads its comments again, which also
finishes an import that reported github_import_incomplete.`,
	Args: exactArgs(1),
	RunE: runIssueImport,
}

func init() {
	issueCmd.AddCommand(issueImportCmd)
	issueImportCmd.Flags().String("output", "json", "Output format: table or json")
}

func runIssueImport(cmd *cobra.Command, args []string) error {
	ref := strings.TrimSpace(args[0])
	if ref == "" {
		return fmt.Errorf("a GitHub issue is required: owner/repo#N or its URL")
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()

	var issue map[string]any
	if err := client.PostJSON(ctx, "/api/issues/import", map[string]any{"ref": ref}, &issue); err != nil {
		return fmt.Errorf("import issue: %w", err)
	}

	output, _ := cmd.Flags().GetString("output")
	if output == "table" {
		cli.PrintTable(os.Stdout, []string{"KEY", "TITLE", "STATUS"}, [][]string{{
			issueDisplayKey(issue), strVal(issue, "title"), strVal(issue, "status"),
		}})
		return nil
	}
	return cli.PrintJSON(os.Stdout, issue)
}
