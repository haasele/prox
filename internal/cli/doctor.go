package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

type doctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type doctorReport struct {
	Healthy bool          `json:"healthy"`
	Checks  []doctorCheck `json:"checks"`
}

func newDoctorCmd() *cobra.Command {
	var fix, asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check Docker, DOCKER_HOST, CA trust, DNS, ports and enforcement",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, c, err := clientConfig()
			if err != nil {
				return err
			}
			path := "/v1/doctor"
			if fix {
				path += "?fix=1"
			}
			// DOCKER_HOST belongs to the calling shell, not the daemon.
			expected := "unix://" + cfg.SocketPath
			clientCheck := doctorCheck{Name: "DOCKER_HOST", Status: "warn"}
			if got := os.Getenv("DOCKER_HOST"); got == expected {
				clientCheck.Status, clientCheck.Detail = "ok", got
			} else {
				if got == "" {
					got = "(unset)"
				}
				clientCheck.Detail = fmt.Sprintf("DOCKER_HOST=%s; run: export DOCKER_HOST=%s", got, expected)
			}

			var rep doctorReport
			if err := c.Get(context.Background(), path, &rep); err != nil {
				rep = doctorReport{Checks: []doctorCheck{clientCheck}}
				fallback := fmt.Sprintf("fallback: export DOCKER_HOST=unix://%s", cfg.DockerSocket)
				rep.Checks = append([]doctorCheck{{
					Name: "daemon", Status: "fail",
					Detail: err.Error() + "; " + fallback,
				}}, rep.Checks...)
				if _, statErr := os.Stat(cfg.DockerSocket); statErr == nil {
					rep.Checks = append(rep.Checks, doctorCheck{Name: "raw docker socket", Status: "ok", Detail: cfg.DockerSocket})
				} else {
					rep.Checks = append(rep.Checks, doctorCheck{Name: "raw docker socket", Status: "warn", Detail: cfg.DockerSocket + " not found"})
				}
				caPath := filepath.Join(cfg.CAStorageDir, "pki", "authorities", "local", "root.crt")
				if _, statErr := os.Stat(caPath); statErr == nil {
					rep.Checks = append(rep.Checks, doctorCheck{Name: "local CA", Status: "ok", Detail: caPath})
				} else {
					rep.Checks = append(rep.Checks, doctorCheck{Name: "local CA", Status: "warn", Detail: "not created yet; start the daemon"})
				}
			} else {
				rep.Checks = append([]doctorCheck{clientCheck}, rep.Checks...)
				rep.Healthy = true
				for _, chk := range rep.Checks {
					if chk.Status == "fail" {
						rep.Healthy = false
					}
				}
			}
			if asJSON {
				b, _ := jsonMarshal(rep)
				fmt.Println(string(b))
				if !rep.Healthy {
					return &exitError{code: 1}
				}
				return nil
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			for _, chk := range rep.Checks {
				sym := "OK"
				switch chk.Status {
				case "warn":
					sym = "WARN"
				case "fail":
					sym = "FAIL"
				}
				fmt.Fprintf(tw, "%-5s\t%s\t%s\n", sym, chk.Name, chk.Detail)
			}
			tw.Flush()
			fmt.Println()
			if rep.Healthy {
				fmt.Println("prox is healthy")
				return nil
			}
			return &exitError{code: 1, err: fmt.Errorf("prox doctor found problems")}
		},
	}
	cmd.Flags().BoolVar(&fix, "fix", false, "apply safe fixes (DNS split config, CA install, prox network)")
	cmd.Flags().BoolVarP(&asJSON, "json", "j", false, "output JSON")
	return cmd
}
