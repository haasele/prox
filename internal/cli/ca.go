package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/haasele/prox/internal/ca"
	"github.com/spf13/cobra"
)

func newCACmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ca",
		Short: "Manage the local certificate authority",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "install",
		Short: "Trust the local CA in the system and NSS stores",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			pem, err := fetchRootPEM(context.Background())
			if err != nil {
				return err
			}
			sys := ca.InstallSystem(pem)
			nss := ca.InstallNSS(pem)
			if sys.Applied {
				fmt.Println("system trust store: installed")
			} else {
				fmt.Println("system trust store: manual step required")
				for _, c := range sys.Commands {
					fmt.Println("  " + c)
				}
			}
			for _, d := range sys.Details {
				fmt.Println("  - " + d)
			}
			if nss.Applied {
				fmt.Println("NSS (Chrome/Firefox): installed")
			} else {
				fmt.Println("NSS (Chrome/Firefox): manual step required")
				for _, c := range nss.Commands {
					fmt.Println("  " + c)
				}
			}
			for _, d := range nss.Details {
				fmt.Println("  - " + d)
			}
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "uninstall",
		Short: "Remove the local CA from trust stores",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			sys := ca.UninstallSystem()
			nss := ca.UninstallNSS()
			if sys.Applied {
				fmt.Println("system trust store: removed")
			} else {
				for _, c := range sys.Commands {
					fmt.Println("  " + c)
				}
			}
			if nss.Applied {
				fmt.Println("NSS (Chrome/Firefox): removed")
			}
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Print the local root certificate (PEM)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			pem, err := fetchRootPEM(context.Background())
			if err != nil {
				return err
			}
			_, err = os.Stdout.Write(pem)
			return err
		},
	})
	return cmd
}

func fetchRootPEM(ctx context.Context) ([]byte, error) {
	cfg, c, err := clientConfig()
	if err != nil {
		return nil, err
	}
	if pem, err := c.GetBytes(ctx, "/v1/ca"); err == nil && len(pem) > 0 {
		return pem, nil
	}
	path := filepath.Join(cfg.CAStorageDir, "pki", "authorities", "local", "root.crt")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("local CA not available (daemon down and %s missing): %w", path, err)
	}
	return b, nil
}
