// Added by the mouse9527/multica fork: `project github-settings`, the GitHub
// settings of a project for GitHub-native issues
// (server/internal/githubissues). Upstream has no such file.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

var projectGitHubSettingsCmd = &cobra.Command{
	Use:   "github-settings",
	Short: "Show or change a project's GitHub settings (default repository, status labels)",
}

var projectGitHubSettingsGetCmd = &cobra.Command{
	Use:   "get <project>",
	Short: "Show a project's GitHub settings",
	Args:  exactArgs(1),
	RunE:  runProjectGitHubSettingsGet,
}

var projectGitHubSettingsSetCmd = &cobra.Command{
	Use:   "set <project>",
	Short: "Change a project's default repository or status labels",
	Long: `Change a project's GitHub settings. Parts without a flag are kept.

--status-label key=label replaces the whole status label map with the given
pairs (repeat the flag for each status); --clear-status-labels empties it.
A label is "status:<name>"; a key is a status of the project's workflow other
than its entry status and the done and cancelled statuses.`,
	Example: `  multica project github-settings set <project> --default-repo acme/widgets
  multica project github-settings set <project> --status-label todo=status:todo --status-label in_review=status:review`,
	Args: exactArgs(1),
	RunE: runProjectGitHubSettingsSet,
}

func init() {
	projectCmd.AddCommand(projectGitHubSettingsCmd)
	projectGitHubSettingsCmd.AddCommand(projectGitHubSettingsGetCmd)
	projectGitHubSettingsCmd.AddCommand(projectGitHubSettingsSetCmd)

	projectGitHubSettingsGetCmd.Flags().String("output", "table", "Output format: table or json")
	registerProjectGitHubSettingsSetFlags(projectGitHubSettingsSetCmd)
}

func registerProjectGitHubSettingsSetFlags(cmd *cobra.Command) {
	cmd.Flags().String("default-repo", "", "Make this GitHub repository (owner/name) of the project its default for new issues")
	cmd.Flags().Bool("clear-default-repo", false, "Clear the project's default repository")
	cmd.Flags().StringArray("status-label", nil, "Map a status to a GitHub label, as key=status:<name> (repeatable; replaces the whole map)")
	cmd.Flags().Bool("clear-status-labels", false, "Remove every status label mapping")
	cmd.Flags().String("output", "table", "Output format: table or json")
}

// projectGitHubSettings is the body of GET and PUT
// /api/projects/{id}/github-settings.
type projectGitHubSettings struct {
	Repos []struct {
		ResourceID string `json:"resource_id"`
		Repo       string `json:"repo"`
	} `json:"repos"`
	DefaultResourceID *string           `json:"default_resource_id"`
	CreateStatuses    []string          `json:"create_statuses"`
	StatusLabelMap    map[string]string `json:"status_label_map"`
	MappableStatuses  []string          `json:"mappable_statuses"`
}

func runProjectGitHubSettingsGet(cmd *cobra.Command, args []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	projectRef, err := resolveProjectID(ctx, client, args[0])
	if err != nil {
		return fmt.Errorf("resolve project: %w", err)
	}
	var raw json.RawMessage
	if err := client.GetJSON(ctx, "/api/projects/"+projectRef.ID+"/github-settings", &raw); err != nil {
		return projectGitHubSettingsError("get github settings", err)
	}
	return printProjectGitHubSettings(cmd, raw)
}

// projectGitHubSettingsBody turns the set flags into the PUT body. The
// project's settings are read only to find the resource of --default-repo.
func projectGitHubSettingsBody(cmd *cobra.Command, current func() (projectGitHubSettings, error)) (map[string]any, error) {
	defaultRepo, _ := cmd.Flags().GetString("default-repo")
	clearDefault, _ := cmd.Flags().GetBool("clear-default-repo")
	pairs, _ := cmd.Flags().GetStringArray("status-label")
	clearLabels, _ := cmd.Flags().GetBool("clear-status-labels")
	defaultRepo = strings.TrimSpace(defaultRepo)

	if defaultRepo != "" && clearDefault {
		return nil, errors.New("--default-repo cannot be combined with --clear-default-repo")
	}
	if len(pairs) > 0 && clearLabels {
		return nil, errors.New("--status-label cannot be combined with --clear-status-labels")
	}
	if defaultRepo == "" && !clearDefault && len(pairs) == 0 && !clearLabels {
		return nil, errors.New("nothing to change; pass --default-repo, --clear-default-repo, --status-label or --clear-status-labels")
	}

	body := map[string]any{}
	if clearLabels {
		body["status_label_map"] = map[string]string{}
	}
	if len(pairs) > 0 {
		labels := map[string]string{}
		for _, pair := range pairs {
			key, label, ok := strings.Cut(pair, "=")
			key, label = strings.TrimSpace(key), strings.TrimSpace(label)
			if !ok || key == "" || label == "" {
				return nil, fmt.Errorf("invalid --status-label %q; expected key=status:<name>", pair)
			}
			if _, dup := labels[key]; dup {
				return nil, fmt.Errorf("--status-label gives status %q twice", key)
			}
			labels[key] = label
		}
		body["status_label_map"] = labels
	}
	if clearDefault {
		body["default_resource_id"] = nil
	}
	if defaultRepo != "" {
		owner, name, ok := strings.Cut(defaultRepo, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return nil, fmt.Errorf("invalid --default-repo %q; expected owner/name", defaultRepo)
		}
		settings, err := current()
		if err != nil {
			return nil, err
		}
		var names []string
		for _, repo := range settings.Repos {
			if strings.EqualFold(repo.Repo, defaultRepo) {
				body["default_resource_id"] = repo.ResourceID
			}
			names = append(names, repo.Repo)
		}
		if _, found := body["default_resource_id"]; !found {
			if len(names) == 0 {
				return nil, fmt.Errorf("--default-repo %s: the project has no GitHub repositories; add one with `multica project resource add`", defaultRepo)
			}
			return nil, fmt.Errorf("--default-repo %s is not a GitHub repository of the project; its repositories: %s", defaultRepo, strings.Join(names, ", "))
		}
	}
	return body, nil
}

func runProjectGitHubSettingsSet(cmd *cobra.Command, args []string) error {
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	projectRef, err := resolveProjectID(ctx, client, args[0])
	if err != nil {
		return fmt.Errorf("resolve project: %w", err)
	}
	path := "/api/projects/" + projectRef.ID + "/github-settings"
	body, err := projectGitHubSettingsBody(cmd, func() (projectGitHubSettings, error) {
		var settings projectGitHubSettings
		if err := client.GetJSON(ctx, path, &settings); err != nil {
			return settings, projectGitHubSettingsError("get github settings", err)
		}
		return settings, nil
	})
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := client.PutJSON(ctx, path, body, &raw); err != nil {
		return projectGitHubSettingsError("set github settings", err)
	}
	return printProjectGitHubSettings(cmd, raw)
}

// projectGitHubSettingsError passes the server's refusal on as it is, with
// the statuses that may be mapped, and explains a 404 from a server with
// GitHub-native issues off.
func projectGitHubSettingsError(action string, err error) error {
	var httpErr *cli.HTTPError
	if !errors.As(err, &httpErr) {
		return fmt.Errorf("%s: %w", action, err)
	}
	var payload struct {
		Code     string   `json:"code"`
		Error    string   `json:"error"`
		Mappable []string `json:"mappable_statuses"`
	}
	parsed := json.Unmarshal([]byte(httpErr.Body), &payload) == nil
	switch {
	case httpErr.StatusCode == http.StatusNotFound && (!parsed || payload.Error == "" || strings.Contains(strings.ToLower(payload.Error), "page not found")):
		return fmt.Errorf("%s: this server does not offer GitHub settings (GitHub-native issues are off): %w", action, err)
	case parsed && strings.HasPrefix(payload.Code, "github_status_label_"):
		msg := fmt.Sprintf("%s: %s [%s]", action, payload.Error, payload.Code)
		if len(payload.Mappable) == 0 {
			msg += "\n  the project's workflow has no status a label can stand for"
		} else {
			msg += "\n  statuses that can be mapped: " + strings.Join(payload.Mappable, ", ")
		}
		return errors.New(msg)
	}
	return fmt.Errorf("%s: %w", action, err)
}

func printProjectGitHubSettings(cmd *cobra.Command, raw json.RawMessage) error {
	output, _ := cmd.Flags().GetString("output")
	if output == "json" {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		return cli.PrintJSON(os.Stdout, v)
	}
	var settings projectGitHubSettings
	if err := json.Unmarshal(raw, &settings); err != nil {
		return err
	}
	writeProjectGitHubSettings(os.Stdout, settings)
	return nil
}

func writeProjectGitHubSettings(w io.Writer, settings projectGitHubSettings) {
	def := "(none)"
	var repos []string
	for _, repo := range settings.Repos {
		repos = append(repos, repo.Repo)
		if settings.DefaultResourceID != nil && *settings.DefaultResourceID == repo.ResourceID {
			def = repo.Repo
		}
	}
	orNone := func(list []string) string {
		if len(list) == 0 {
			return "(none)"
		}
		return strings.Join(list, ", ")
	}
	fmt.Fprintf(w, "Repositories:       %s\n", orNone(repos))
	fmt.Fprintf(w, "Default repository: %s\n", def)
	fmt.Fprintf(w, "Create statuses:    %s\n", orNone(settings.CreateStatuses))
	fmt.Fprintf(w, "Mappable statuses:  %s\n", orNone(settings.MappableStatuses))
	if len(settings.StatusLabelMap) == 0 {
		fmt.Fprintln(w, "Status labels:      (none)")
		return
	}
	fmt.Fprintln(w, "Status labels:")
	keys := make([]string, 0, len(settings.StatusLabelMap))
	for key := range settings.StatusLabelMap {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	rows := make([][]string, 0, len(keys))
	for _, key := range keys {
		rows = append(rows, []string{key, settings.StatusLabelMap[key]})
	}
	cli.PrintTable(w, []string{"STATUS", "LABEL"}, rows)
}
