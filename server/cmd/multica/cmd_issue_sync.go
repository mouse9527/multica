// Added by the mouse9527/multica fork: syncing GitHub issues on request.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

var issueSyncCmd = &cobra.Command{
	Use:   "sync [<issue> | --project <project>]",
	Short: "Sync GitHub issues from GitHub now",
	Long: `Read a GitHub issue from GitHub now and store what changed: its title,
description, state and labels, new and edited comments, and the pull requests
linked to it. GitHub is not polled by default, so sync an issue before working
on it or deciding on it.

<issue> is the issue key (owner/repo#N or MUL-123) or its UUID. With
--project, every GitHub issue of the project is synced one after another
(--open-only skips those closed on GitHub); the run stops as soon as the
server's quota guard refuses a sync and reports how far it got.

A sync of one issue costs about 3 GitHub requests: the issue, one per 100
comments, and one per 100 timeline events plus one per changed linked pull
request.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runIssueSync,
}

func init() {
	issueCmd.AddCommand(issueSyncCmd)
	issueSyncCmd.Flags().String("project", "", "Sync every GitHub issue of this project")
	issueSyncCmd.Flags().Bool("open-only", false, "With --project, skip issues closed on GitHub")
	issueSyncCmd.Flags().String("output", "json", "Output format: table or json")
}

func runIssueSync(cmd *cobra.Command, args []string) error {
	project, _ := cmd.Flags().GetString("project")
	project = strings.TrimSpace(project)
	openOnly, _ := cmd.Flags().GetBool("open-only")
	switch {
	case len(args) == 1 && project != "":
		return errors.New("pass either an issue or --project, not both")
	case len(args) == 0 && project == "":
		return errors.New("an issue (owner/repo#N, key or UUID) or --project is required")
	case len(args) == 1 && openOnly:
		return errors.New("--open-only applies to --project only")
	}
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	output, _ := cmd.Flags().GetString("output")
	if project != "" {
		return runProjectSync(cmd.Context(), client, project, openOnly, output)
	}

	ref := strings.TrimSpace(args[0])
	if ref == "" {
		return errors.New("an issue (owner/repo#N, key or UUID) is required")
	}
	result, err := syncOneIssue(cmd.Context(), client, ref)
	if err != nil {
		return fmt.Errorf("sync issue: %w", err)
	}
	if output == "table" {
		cli.PrintTable(os.Stdout, syncTableHeader, [][]string{syncTableRow(result)})
		return nil
	}
	return cli.PrintJSON(os.Stdout, result)
}

func syncOneIssue(parent context.Context, client *cli.APIClient, ref string) (map[string]any, error) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := cli.APIContext(parent)
	defer cancel()
	var result map[string]any
	err := client.PostJSON(ctx, "/api/issues/"+url.PathEscape(ref)+"/github-sync", map[string]any{}, &result)
	return result, err
}

var syncTableHeader = []string{"KEY", "ISSUE", "NEW COMMENTS", "EDITED COMMENTS", "PULLS", "REQUESTS"}

func syncTableRow(r map[string]any) []string {
	issue := "unchanged"
	if gone := strVal(r, "gone"); gone != "" {
		issue = gone
	} else if b, _ := r["issue_updated"].(bool); b {
		issue = "updated"
	}
	return []string{strVal(r, "identifier"), issue, numVal(r, "new_comments"), numVal(r, "comments_updated"),
		numVal(r, "pulls_updated"), numVal(r, "requests_used")}
}

// syncStopCodes are the refusals that hold for every issue after them, so
// a project sync stops there instead of spending requests on them.
var syncStopCodes = map[string]bool{
	"github_quota_low": true, "github_rate_limited": true, "github_token_unusable": true,
}

// projectSyncSummary is the result of a project sync.
type projectSyncSummary struct {
	ProjectID    string           `json:"project_id"`
	Total        int              `json:"total"`
	Synced       int              `json:"synced"`
	Changed      int              `json:"changed"`
	Failed       int              `json:"failed"`
	NotAttempted int              `json:"not_attempted"`
	RequestsUsed int              `json:"requests_used"`
	Stopped      map[string]any   `json:"stopped,omitempty"`
	RateLimit    any              `json:"rate_limit,omitempty"`
	Results      []map[string]any `json:"results"`
}

func runProjectSync(ctx context.Context, client *cli.APIClient, projectRef string, openOnly bool, output string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	listCtx, cancel := cli.APIContext(ctx)
	project, err := resolveProjectID(listCtx, client, projectRef)
	if err != nil {
		cancel()
		return fmt.Errorf("resolve project: %w", err)
	}
	var list struct {
		Issues []struct {
			ID         string `json:"id"`
			Identifier string `json:"identifier"`
		} `json:"issues"`
	}
	path := "/api/projects/" + url.PathEscape(project.ID) + "/github-issues"
	if openOnly {
		path += "?open_only=true"
	}
	err = client.GetJSON(listCtx, path, &list)
	cancel()
	if err != nil {
		return fmt.Errorf("list the project's GitHub issues: %w", err)
	}

	summary := projectSyncSummary{ProjectID: project.ID, Total: len(list.Issues), Results: []map[string]any{}}
	for i, issue := range list.Issues {
		result, err := syncOneIssue(ctx, client, issue.ID)
		if err == nil {
			summary.Synced++
			if syncChanged(result) {
				summary.Changed++
			}
			summary.RequestsUsed += intVal(result, "requests_used")
			if rl, ok := result["rate_limit"]; ok {
				summary.RateLimit = rl
			}
			summary.Results = append(summary.Results, result)
			continue
		}
		code, body := syncErrorBody(err)
		summary.RequestsUsed += intVal(body, "requests_used")
		if syncStopCodes[code] {
			body["identifier"] = issue.Identifier
			summary.Stopped = body
			summary.NotAttempted = len(list.Issues) - i
			break
		}
		summary.Failed++
		summary.Results = append(summary.Results, map[string]any{"identifier": issue.Identifier, "issue_id": issue.ID, "error": syncErrorText(err, body)})
	}

	if output == "table" {
		rows := make([][]string, 0, len(summary.Results))
		for _, r := range summary.Results {
			if msg := strVal(r, "error"); msg != "" {
				rows = append(rows, []string{strVal(r, "identifier"), "failed: " + msg, "", "", "", ""})
				continue
			}
			rows = append(rows, syncTableRow(r))
		}
		cli.PrintTable(os.Stdout, syncTableHeader, rows)
		fmt.Fprintf(os.Stdout, "\nsynced %d of %d, %d changed, %d failed, %d GitHub requests\n",
			summary.Synced, summary.Total, summary.Changed, summary.Failed, summary.RequestsUsed)
	} else if err := cli.PrintJSON(os.Stdout, summary); err != nil {
		return err
	}
	switch {
	case summary.Stopped != nil:
		return fmt.Errorf("stopped after %d of %d issues: %s (retry after %v seconds)", summary.Synced+summary.Failed,
			summary.Total, strVal(summary.Stopped, "error"), summary.Stopped["retry_after_seconds"])
	case summary.Failed > 0:
		return fmt.Errorf("%d of %d issues failed to sync", summary.Failed, summary.Total)
	}
	return nil
}

func syncChanged(r map[string]any) bool {
	updated, _ := r["issue_updated"].(bool)
	return updated || strVal(r, "gone") != "" ||
		intVal(r, "new_comments") > 0 || intVal(r, "comments_updated") > 0 || intVal(r, "pulls_updated") > 0
}

func intVal(m map[string]any, key string) int {
	f, _ := m[key].(float64)
	return int(f)
}

// syncErrorBody reads the server's structured refusal; body is empty when the
// error carries none.
func syncErrorBody(err error) (code string, body map[string]any) {
	body = map[string]any{}
	var httpErr *cli.HTTPError
	if errors.As(err, &httpErr) && json.Unmarshal([]byte(httpErr.Body), &body) == nil {
		code = strVal(body, "code")
		return code, body
	}
	return "", map[string]any{}
}

func syncErrorText(err error, body map[string]any) string {
	if msg := strVal(body, "error"); msg != "" {
		if code := strVal(body, "code"); code != "" {
			return msg + " [" + code + "]"
		}
		return msg
	}
	return err.Error()
}
