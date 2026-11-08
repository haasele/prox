package cli

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

type enforcementJSON struct {
	Enabled      bool     `json:"enabled"`
	Running      bool     `json:"running"`
	Socket       string   `json:"socket"`
	Error        string   `json:"error"`
	Instructions []string `json:"instructions"`
}

func newEnforceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "enforce",
		Short: "Control Docker AuthZ enforcement mode",
	}
	status := func() error {
		_, c, err := clientConfig()
		if err != nil {
			return err
		}
		var st enforcementJSON
		if err := c.Get(context.Background(), "/v1/enforce", &st); err != nil {
			return err
		}
		printEnforce(st)
		return nil
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "on",
		Short: "Start the AuthZ plugin and print Docker daemon setup steps",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, c, err := clientConfig()
			if err != nil {
				return err
			}
			var st enforcementJSON
			if err := c.Post(context.Background(), "/v1/enforce", map[string]bool{"enabled": true}, &st); err != nil {
				if st.Error != "" {
					printEnforce(st)
				}
				return err
			}
			printEnforce(st)
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "off",
		Short: "Disable enforcement",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, c, err := clientConfig()
			if err != nil {
				return err
			}
			var st enforcementJSON
			if err := c.Post(context.Background(), "/v1/enforce", map[string]bool{"enabled": false}, &st); err != nil {
				return err
			}
			printEnforce(st)
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show enforcement status",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return status() },
	})
	return cmd
}

func printEnforce(st enforcementJSON) {
	state := "off"
	if st.Enabled {
		state = "on"
	}
	run := "not running"
	if st.Running {
		run = "listening on " + st.Socket
	}
	fmt.Printf("enforcement %s (%s)\n", state, run)
	if st.Error != "" {
		fmt.Println("error: " + st.Error)
	}
	if len(st.Instructions) > 0 {
		fmt.Println("\nDocker daemon setup:")
		for _, i := range st.Instructions {
			fmt.Println("  - " + i)
		}
	}
}

func jsonMarshal(v any) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}
