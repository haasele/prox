package cli

import (
	"context"
	"fmt"

	"github.com/haasele/prox/internal/dns"
	"github.com/spf13/cobra"
)

// newHostsCmd is the optional /etc/hosts fallback. It is hidden and never runs
// automatically; split DNS is the preferred mechanism.
func newHostsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "hosts",
		Short:  "Optional /etc/hosts fallback (off by default)",
		Hidden: true,
	}
	syncCmd := &cobra.Command{
		Use:   "sync",
		Short: "Write current prox routes to a managed /etc/hosts block (needs root)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, c, err := clientConfig()
			if err != nil {
				return err
			}
			var routes []routeJSON
			if err := c.Get(context.Background(), "/v1/routes", &routes); err != nil {
				return err
			}
			hosts := []string{"gw.localhost"}
			for _, r := range routes {
				if r.Proto == "http" || r.Proto == "sni" {
					hosts = append(hosts, r.Host)
				}
			}
			if err := dns.WriteHostsFallback("/etc/hosts", hosts); err != nil {
				return fmt.Errorf("write /etc/hosts (try: sudo gw hosts sync): %w", err)
			}
			fmt.Printf("updated /etc/hosts with %d prox hostname(s)\n", len(hosts))
			return nil
		},
	}
	cmd.AddCommand(syncCmd)
	return cmd
}
