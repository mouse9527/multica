// Added by the mouse9527/multica fork: the Super Agent guide in the CLI (M-150).

package main

import (
	_ "embed"
	"fmt"

	"github.com/spf13/cobra"
)

// routerGuide is the one source of the Super Agent guide: when to act, which
// command each owner request maps to, what to confirm first, how to report a
// failure, and the Super Agent's boundary. It ships inside the CLI so every
// workspace reads the version that matches the commands it describes, with
// nothing to import or attach.
//
//go:embed router_guide.md
var routerGuide string

var routerGuideCmd = &cobra.Command{
	Use:   "guide",
	Short: "Print the Super Agent guide: owner requests to commands, confirmation, failures, and the Super Agent's boundary",
	Args:  exactArgs(0),
	RunE: func(cmd *cobra.Command, _ []string) error {
		_, err := fmt.Fprint(cmd.OutOrStdout(), routerGuide)
		return err
	},
}

func init() {
	routerCmd.AddCommand(routerGuideCmd)
}
