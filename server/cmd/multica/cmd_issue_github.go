// Added by the mouse9527/multica fork: choosing where `issue create` puts a
// GitHub-native issue (server/internal/githubissues). Upstream has no such
// file.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

func registerIssueCreateGitHubFlags(cmd *cobra.Command) {
	cmd.Flags().String("repo", "", "GitHub repository (owner/name) to open the issue on. In a project with GitHub repositories, agents must pass --repo or --provider local")
	cmd.Flags().String("provider", "", "Where the issue lives: local (never synced to GitHub) or github")
}

// applyIssueCreateGitHubFlags copies --repo and --provider into the create
// body. A server without GitHub-native issues ignores both.
func applyIssueCreateGitHubFlags(cmd *cobra.Command, body map[string]any) error {
	repo, _ := cmd.Flags().GetString("repo")
	provider, _ := cmd.Flags().GetString("provider")
	repo, provider = strings.TrimSpace(repo), strings.TrimSpace(provider)
	switch provider {
	case "", "local", "github":
	default:
		return fmt.Errorf("invalid --provider %q; valid values: local, github", provider)
	}
	if repo != "" && provider == "local" {
		return errors.New("--repo cannot be combined with --provider local")
	}
	if repo != "" {
		owner, name, ok := strings.Cut(repo, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return fmt.Errorf("invalid --repo %q; expected owner/name", repo)
		}
		body["repo"] = repo
	}
	if provider != "" {
		body["provider"] = provider
	}
	return nil
}

// githubRepoChoiceMessage turns the server's refusal to choose a repository
// into a message naming the candidates and the flags that settle it.
func githubRepoChoiceMessage(err error) (string, bool) {
	var httpErr *cli.HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusUnprocessableEntity {
		return "", false
	}
	var payload struct {
		Code       string   `json:"code"`
		Error      string   `json:"error"`
		Candidates []string `json:"candidates"`
		Default    string   `json:"default_repo"`
	}
	if json.Unmarshal([]byte(httpErr.Body), &payload) != nil {
		return "", false
	}
	switch payload.Code {
	case "github_repo_required", "github_repo_ambiguous", "github_repo_not_registered":
	default:
		return "", false
	}
	var b strings.Builder
	fmt.Fprintf(&b, "create issue: %s [%s]", payload.Error, payload.Code)
	for _, repo := range payload.Candidates {
		mark := ""
		if repo == payload.Default {
			mark = " (project default)"
		}
		fmt.Fprintf(&b, "\n  --repo %s%s", repo, mark)
	}
	b.WriteString("\n  --provider local   (a local issue, not synced to GitHub)")
	return b.String(), true
}
