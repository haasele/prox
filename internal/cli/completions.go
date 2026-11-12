package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newCompletionsCmd(root *cobra.Command) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "completions",
		Short: "Generate shell completions",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "bash",
		Short: "Print bash completion script",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return root.GenBashCompletion(cmd.OutOrStdout())
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "zsh",
		Short: "Print zsh completion script",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return root.GenZshCompletion(cmd.OutOrStdout())
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "fish",
		Short: "Print fish completion script",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return root.GenFishCompletion(cmd.OutOrStdout(), true)
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "man",
		Short: "Print a man page skeleton",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), ".TH PROX 1")
			fmt.Fprintln(cmd.OutOrStdout(), ".SH NAME")
			fmt.Fprintln(cmd.OutOrStdout(), "prox \\- prox CLI and daemon")
			return nil
		},
	})
	return cmd
}
