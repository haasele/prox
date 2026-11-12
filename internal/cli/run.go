package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/haasele/prox/internal/hints"
	"github.com/haasele/prox/internal/hostproc"
	"github.com/haasele/prox/internal/policy"
	"github.com/spf13/cobra"
)

type leaseResponse struct {
	Lease leaseJSON `json:"lease"`
}

func newRunCmd() *cobra.Command {
	var resolve, portFlag, name, dir string
	cmd := &cobra.Command{
		Use:   "run [flags] -- <command...>",
		Short: "Run a local dev process behind the gateway",
		Example: `  prox run -- npm run dev
  prox run --resolve app -- npm run dev
  prox run -- python manage.py runserver
  prox run --port-flag '--listen={host}:{port}' -- ./server`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			workDir := dir
			if workDir == "" {
				wd, err := os.Getwd()
				if err != nil {
					return err
				}
				workDir = wd
			}
			abs, err := filepath.Abs(workDir)
			if err != nil {
				return err
			}
			workDir = abs
			cfg, client, err := clientConfig()
			if err != nil {
				return err
			}
			leaseName := name
			if leaseName == "" {
				leaseName = policy.SanitizeLabel(filepath.Base(workDir))
			}
			host := resolve
			if host == "" {
				host = leaseName
			}
			host = policy.NormalizeResolve(host, cfg.ProjectDomain)

			det := hostproc.Detect(workDir, args)
			port, err := hostproc.FreePort()
			if err != nil {
				return err
			}
			childArgs := append([]string{}, args...)
			childArgs = append(childArgs, hostproc.PortArgs(args, det, port, portFlag)...)
			child := exec.Command(childArgs[0], childArgs[1:]...)
			child.Dir = workDir
			child.Stdin = os.Stdin
			child.Stdout = os.Stdout
			child.Stderr = os.Stderr
			child.Env = hostproc.Environment(hostproc.Env(port, host, det))
			if err := child.Start(); err != nil {
				return fmt.Errorf("start %s: %w", childArgs[0], err)
			}
			registered := false
			var leaseID int64
			defer func() {
				if registered {
					_ = client.Delete(context.Background(), fmt.Sprintf("/v1/leases/%d", leaseID), nil)
				}
			}()
			var resp leaseResponse
			req := map[string]any{
				"name":    leaseName,
				"host":    host,
				"port":    port,
				"pid":     child.Process.Pid,
				"dir":     workDir,
				"command": hostproc.CommandString(args),
			}
			if err := client.Post(context.Background(), "/v1/leases", req, &resp); err != nil {
				_ = child.Process.Kill()
				_, _ = child.Process.Wait()
				return fmt.Errorf("register lease: %w", err)
			}
			registered = true
			leaseID = resp.Lease.ID
			publicURL := "https://" + cfg.HTTPSHostPort(resp.Lease.Host)
			fmt.Printf("prox: %s  (%s, pid %d)\n",
				policy.RouteDescription("", "http", resp.Lease.Host, port, 443, cfg.HTTPSPort), det.Adapter.Name, child.Process.Pid)
			if hp := det.Adapter.DefaultPort; hp > 0 && hp != port {
				hintMgr := hints.New(slog.Default())
				defer hintMgr.Close()
				for _, ch := range hintMgr.Reconcile([]hints.Hint{{Port: hp, Name: leaseName, URL: publicURL}}) {
					if ch.Err != nil {
						fmt.Fprintf(os.Stderr, "prox: port %d is busy; no original-port hint\n", hp)
					} else if ch.Started {
						fmt.Printf("prox: http://localhost:%d shows the hint page -> %s\n", hp, publicURL)
					}
				}
			}
			werr := child.Wait()
			if werr != nil {
				if ee, ok := werr.(*exec.ExitError); ok {
					if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
						return &exitError{code: 128 + int(ws.Signal())}
					}
					return &exitError{code: ee.ExitCode()}
				}
				return werr
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&resolve, "resolve", "", "hostname (default <directory>.localhost)")
	cmd.Flags().StringVar(&portFlag, "port-flag", "", "override port flag template, e.g. '--port={port}'")
	cmd.Flags().StringVar(&name, "name", "", "lease name (default directory name)")
	cmd.Flags().StringVar(&dir, "dir", "", "working directory (default current directory)")
	return cmd
}
