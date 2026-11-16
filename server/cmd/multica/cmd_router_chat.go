// Added by the mouse9527/multica fork: Router settings by instruction (M-105).

package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

// A Router is a chat plus the subscriptions bound to it. These helpers let
// the router commands name a Router by its chat: `--chat current` inside a
// chat run (the owner said "this chat"), or a chat session UUID.

const currentChat = "current"

var routerReleaseCmd = &cobra.Command{
	Use:   "release",
	Short: "Stop a chat being a Router: delete its subscriptions, or with --keep pause and unbind them",
	Args:  exactArgs(0),
	RunE:  runRouterRelease,
}

func init() {
	routerCmd.AddCommand(routerReleaseCmd)
	routerReleaseCmd.Flags().String("chat", "", "The Router's chat: current (inside a chat run) or a chat session UUID (required)")
	routerReleaseCmd.Flags().Bool("keep", false, "Keep the subscriptions, paused and unbound, instead of deleting them")
}

type routerChat struct {
	ID    string
	Title string
}

// resolveRouterChat turns a --chat / --anchor-chat value into a chat
// session. "current" asks the server for the chat of the calling run, which
// fails outside a chat run.
func resolveRouterChat(ctx context.Context, client *cli.APIClient, ref string) (routerChat, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return routerChat{}, fmt.Errorf("a chat is required: current (inside a chat run) or a chat session UUID")
	}
	if ref != currentChat {
		return routerChat{ID: ref}, nil
	}
	var chat map[string]any
	if err := client.GetJSON(ctx, "/api/router/chats/current", &chat); err != nil {
		return routerChat{}, fmt.Errorf("resolve the current chat: %w", err)
	}
	return routerChat{ID: strVal(chat, "chat_session_id"), Title: strVal(chat, "title")}, nil
}

// routerChatSubscriptions lists the subscriptions bound to chatID.
func routerChatSubscriptions(ctx context.Context, client *cli.APIClient, chatID string) ([]map[string]any, error) {
	var result struct {
		Subscriptions []map[string]any `json:"subscriptions"`
	}
	path := "/api/router/subscriptions?" + url.Values{"chat_session_id": {chatID}}.Encode()
	if err := client.GetJSON(ctx, path, &result); err != nil {
		return nil, fmt.Errorf("list the Router's subscriptions: %w", err)
	}
	return result.Subscriptions, nil
}

// routerTargets returns the subscriptions a command acts on: the one named
// by its argument, or with --chat every subscription of that Router.
func routerTargets(ctx context.Context, cmd *cobra.Command, client *cli.APIClient, args []string) ([]string, error) {
	chatRef, _ := cmd.Flags().GetString("chat")
	switch {
	case len(args) == 1 && chatRef == "":
		id, err := resolveRouterSubscriptionID(ctx, client, args[0])
		if err != nil {
			return nil, err
		}
		return []string{id}, nil
	case len(args) == 0 && chatRef != "":
		chat, err := resolveRouterChat(ctx, client, chatRef)
		if err != nil {
			return nil, err
		}
		subs, err := routerChatSubscriptions(ctx, client, chat.ID)
		if err != nil {
			return nil, err
		}
		if len(subs) == 0 {
			return nil, fmt.Errorf("chat %s is not a Router: no subscription is bound to it", chat.ID)
		}
		ids := make([]string, 0, len(subs))
		for _, s := range subs {
			ids = append(ids, strVal(s, "id"))
		}
		return ids, nil
	default:
		return nil, fmt.Errorf("pass a subscription id, or --chat for every subscription of a Router")
	}
}

// printRouterSubscriptions prints what a command changed: one subscription
// as before, several (--chat) as a list.
func printRouterSubscriptions(cmd *cobra.Command, subs []map[string]any) error {
	if len(subs) == 1 {
		return printRouterSubscription(cmd, subs[0])
	}
	if output, _ := cmd.Flags().GetString("output"); output == "table" {
		cli.PrintTable(os.Stdout, routerSubscriptionHeaders, routerSubscriptionRows(subs, true))
		return nil
	}
	return cli.PrintJSON(os.Stdout, subs)
}

// routerChatBoard creates the board issue a chat-bound project subscription
// uses as its anchor: dispatch records and the private-project fence key on
// the anchor, so it lives in that project. The title follows the zh-Hans
// web settings; the web uses the viewer's language, the CLI has none.
func routerChatBoard(ctx context.Context, client *cli.APIClient, projectID string, chat routerChat) (string, error) {
	name := strings.TrimSpace(chat.Title)
	if name == "" {
		name = chat.ID
	}
	var issue map[string]any
	if err := client.PostJSON(ctx, "/api/issues", map[string]any{
		"title":       "Router 看板 · " + name,
		"description": "Records the runs of the Router in the chat \"" + name + "\". Created by `multica router subscribe`.",
		"project_id":  projectID,
	}, &issue); err != nil {
		return "", fmt.Errorf("create the board issue: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Created board issue %s as the anchor of this project scope\n", strVal(issue, "identifier"))
	return strVal(issue, "id"), nil
}

func runRouterRelease(cmd *cobra.Command, _ []string) error {
	chatRef, _ := cmd.Flags().GetString("chat")
	if chatRef == "" {
		return fmt.Errorf("--chat is required: current (inside a chat run) or a chat session UUID")
	}
	keep, _ := cmd.Flags().GetBool("keep")
	client, err := newAPIClient(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := cli.APIContext(context.Background())
	defer cancel()
	ids, err := routerTargets(ctx, cmd, client, nil)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if !keep {
			if err := client.DeleteJSON(ctx, routerSubscriptionPath(id)); err != nil {
				return fmt.Errorf("delete subscription %s: %w", id, err)
			}
			fmt.Fprintf(os.Stderr, "Deleted subscription %s\n", id)
			continue
		}
		// Pause before unbinding, so the subscription never falls back to
		// anchor runs in between.
		var sub map[string]any
		if err := client.GetJSON(ctx, routerSubscriptionPath(id), &sub); err != nil {
			return fmt.Errorf("get subscription %s: %w", id, err)
		}
		if enabled, _ := sub["enabled"].(bool); enabled {
			if err := client.PostJSON(ctx, routerSubscriptionPath(id)+"/pause", map[string]any{}, &sub); err != nil {
				return fmt.Errorf("pause subscription %s: %w", id, err)
			}
		}
		if err := client.PatchJSON(ctx, routerSubscriptionPath(id), map[string]any{"anchor_chat_session_id": "none"}, &sub); err != nil {
			return fmt.Errorf("unbind subscription %s: %w", id, err)
		}
		fmt.Fprintf(os.Stderr, "Paused and unbound subscription %s\n", id)
	}
	return nil
}
