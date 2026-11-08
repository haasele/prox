package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

type checkOutcome struct {
	Service     string `json:"service"`
	Image       string `json:"image"`
	Project     string `json:"project"`
	Action      string `json:"action"`
	Status      string `json:"status"`
	Host        string `json:"host"`
	Proto       string `json:"proto"`
	SourcePort  int    `json:"source_port"`
	GatewayPort int    `json:"gateway_port"`
	RuleID      int64  `json:"rule_id"`
	Reason      string `json:"reason"`
	Collision   bool   `json:"collision"`
	Preferred   int    `json:"preferred_gateway_port"`
	GatewayBusy bool   `json:"gateway_port_busy"`
}

type checkResponse struct {
	Project  string         `json:"project"`
	Services []checkOutcome `json:"services"`
}

func newCheckCmd() *cobra.Command {
	var project string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "check <compose.yml>",
		Short: "Dry-run a Compose file and show what prox would do",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := args[0]
			var body []byte
			var err error
			abs := path
			if path == "-" {
				body, err = io.ReadAll(os.Stdin)
				abs = "stdin-compose.yml"
			} else {
				abs, err = filepath.Abs(path)
				if err == nil {
					body, err = os.ReadFile(path)
				}
			}
			if err != nil {
				return err
			}
			_, c, err := clientConfig()
			if err != nil {
				return err
			}
			q := "/v1/check?filename=" + urlQuery(abs)
			if project != "" {
				q += "&project=" + urlQuery(project)
			}
			var resp checkResponse
			if err := c.PostRaw(context.Background(), q, "application/x-yaml", body, &resp); err != nil {
				return err
			}
			if asJSON {
				b, _ := json.MarshalIndent(resp, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			fmt.Printf("project %s\n\n", resp.Project)
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "SERVICE\tIMAGE\tACTION\tHOST\tCONTAINER\tGATEWAY\tRULE\tDETAIL")
			for _, s := range resp.Services {
				host := s.Host
				if host == "" {
					host = "—"
				}
				if s.Collision {
					host += " !"
				}
				container := "—"
				if s.SourcePort > 0 {
					container = strconv.Itoa(s.SourcePort)
				}
				gateway := "—"
				if s.GatewayPort > 0 {
					gateway = strconv.Itoa(s.GatewayPort)
				}
				if s.GatewayBusy {
					gateway += " (busy)"
				}
				rule := "—"
				if s.RuleID > 0 {
					rule = strconv.FormatInt(s.RuleID, 10)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.Service, s.Image, s.Status, host, container, gateway, rule, s.Reason)
			}
			tw.Flush()
			for _, s := range resp.Services {
				if s.Action == "deny" {
					fmt.Printf("\ndenied: %s — %s\n", s.Service, s.Reason)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "override the Compose project name")
	cmd.Flags().BoolVarP(&asJSON, "json", "j", false, "output JSON")
	return cmd
}

func urlQuery(s string) string {
	escaped := ""
	for _, r := range s {
		switch {
		case r == ' ':
			escaped += "%20"
		case r == '&':
			escaped += "%26"
		case r == '?':
			escaped += "%3F"
		case r == '#':
			escaped += "%23"
		case r == '+':
			escaped += "%2B"
		case r == '/':
			escaped += "%2F"
		default:
			escaped += string(r)
		}
	}
	return escaped
}
