package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

func newPolicyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "policy",
		Short: "Show or set the default policy (rewrite|deny|warn)",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "set <rewrite|deny|warn>",
		Short: "Set the default policy",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, c, err := clientConfig()
			if err != nil {
				return err
			}
			var out map[string]string
			if err := c.Post(context.Background(), "/v1/policy", map[string]string{"policy": args[0]}, &out); err != nil {
				return err
			}
			fmt.Println("policy set to " + out["policy"])
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Show the current policy",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, c, err := clientConfig()
			if err != nil {
				return err
			}
			var out map[string]string
			if err := c.Get(context.Background(), "/v1/policy", &out); err != nil {
				return err
			}
			fmt.Println(out["policy"])
			return nil
		},
	})
	return cmd
}
