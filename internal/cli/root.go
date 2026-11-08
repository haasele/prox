package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/haasele/prox/internal/config"
	"github.com/haasele/prox/internal/daemon"
	"github.com/spf13/cobra"
)

type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return fmt.Sprintf("exit %d", e.code)
}

func NewRoot(version string) *cobra.Command {
	root := &cobra.Command{
		Use:           "gw",
		Short:         "prox — one gateway for every container and dev process",
		Long:          "prox routes every Docker container and local dev process through one HTTPS/TCP gateway.\nSet DOCKER_HOST to the prox socket and use *.localhost hostnames.",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version,
	}
	root.SetVersionTemplate("prox {{.Version}}\n")
	root.AddCommand(
		newAllowCmd(),
		newDenyCmd(),
		newLsCmd(),
		newRmCmd(),
		newCheckCmd(),
		newRunCmd(),
		newEventsCmd(),
		newPolicyCmd(),
		newCACmd(),
		newDoctorCmd(),
		newEnforceCmd(),
		newCompletionsCmd(root),
		newHostsCmd(),
		newVersionCmd(version),
		newDaemonCmd(version),
	)
	return root
}

// Main runs the CLI and exits with the appropriate code.
func Main(version string) {
	root := NewRoot(version)
	if err := root.Execute(); err != nil {
		var ee *exitError
		if errors.As(err, &ee) {
			if ee.err != nil {
				fmt.Fprintln(os.Stderr, "gw: "+ee.err.Error())
			}
			os.Exit(ee.code)
		}
		fmt.Fprintln(os.Stderr, "gw: "+err.Error())
		os.Exit(1)
	}
}

func newVersionCmd(version string) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the prox version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("prox " + version)
		},
	}
}

func newDaemonCmd(version string) *cobra.Command {
	var logLevel string
	cmd := &cobra.Command{
		Use:    "daemon",
		Short:  "Run the prox daemon (same as proxd)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if logLevel != "" {
				_ = os.Setenv("PROX_LOG_LEVEL", logLevel)
			}
			return runDaemon(version)
		},
	}
	cmd.Flags().StringVar(&logLevel, "log-level", "", "debug|info|warn|error")
	return cmd
}

func runDaemon(version string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	d, err := daemon.New(ctx, cfg, version)
	if err != nil {
		return err
	}
	if err := d.Start(); err != nil {
		d.Close()
		return err
	}
	d.Log.Info("prox daemon ready", "api", cfg.APISocket, "docker_proxy", cfg.SocketPath, "http", cfg.HTTPPort, "https", cfg.HTTPSPort)
	<-ctx.Done()
	d.Close()
	return nil
}

// DaemonMain is the proxd entrypoint.
func DaemonMain(version string) {
	if err := runDaemon(version); err != nil {
		fmt.Fprintln(os.Stderr, "proxd: "+err.Error())
		os.Exit(1)
	}
}
