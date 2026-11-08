package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

type routeJSON struct {
	ID            int64  `json:"id"`
	Host          string `json:"host"`
	ContainerID   string `json:"container_id"`
	ContainerName string `json:"container_name"`
	Project       string `json:"project"`
	Service       string `json:"service"`
	Proto         string `json:"proto"`
	SourcePort    int    `json:"source_port"`
	GatewayPort   int    `json:"gateway_port"`
	TargetAddr    string `json:"target_addr"`
	RuleID        int64  `json:"rule_id"`
	Status        string `json:"status"`
	Collision     bool   `json:"collision"`
	Redirect      bool   `json:"redirect"`
}

type leaseJSON struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Host    string `json:"host"`
	PID     int    `json:"pid"`
	Port    int    `json:"port"`
	Dir     string `json:"dir"`
	Command string `json:"command"`
}

type statusJSON struct {
	Version      string `json:"version"`
	Policy       string `json:"policy"`
	Backend      string `json:"backend"`
	DockerOK     bool   `json:"docker_ok"`
	DockerError  string `json:"docker_error"`
	Network      string `json:"network"`
	Gateway      string `json:"gateway"`
	HTTPPort     int    `json:"http_port"`
	HTTPSPort    int    `json:"https_port"`
	Enforcement  bool   `json:"enforcement"`
	Routes       int    `json:"routes"`
	Rules        int    `json:"rules"`
	Leases       int    `json:"leases"`
	DashboardURL string `json:"dashboard_url"`
}

type containersJSON struct {
	Warnings []string `json:"warnings"`
}

func newLsCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "Show rules, routes, leases and warnings",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			_, c, err := clientConfig()
			if err != nil {
				return err
			}
			var st statusJSON
			var rules []ruleJSON
			var routes []routeJSON
			var leases []leaseJSON
			var containers containersJSON
			if err := c.Get(ctx, "/v1/status", &st); err != nil {
				return err
			}
			if err := c.Get(ctx, "/v1/rules", &rules); err != nil {
				return err
			}
			if err := c.Get(ctx, "/v1/routes", &routes); err != nil {
				return err
			}
			if err := c.Get(ctx, "/v1/leases", &leases); err != nil {
				return err
			}
			if err := c.Get(ctx, "/v1/containers", &containers); err != nil {
				return err
			}
			if asJSON {
				b, _ := json.MarshalIndent(map[string]any{
					"status": st, "rules": rules, "routes": routes, "leases": leases, "warnings": containers.Warnings,
				}, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			printLs(st, rules, routes, leases, containers.Warnings)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&asJSON, "json", "j", false, "output JSON")
	return cmd
}

func printLs(st statusJSON, rules []ruleJSON, routes []routeJSON, leases []leaseJSON, warnings []string) {
	docker := "ok"
	if !st.DockerOK {
		docker = "unreachable"
	}
	enf := "off"
	if st.Enforcement {
		enf = "on"
	}
	fmt.Printf("policy %s   backend %s   docker %s   enforcement %s\n", st.Policy, st.Backend, docker, enf)
	fmt.Printf("dashboard %s\n\n", st.DashboardURL)

	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "RULES")
	fmt.Fprintln(tw, "ID\tNAME\tPROJECT\tACTION\tSOURCE\tTARGET\tRESOLVE\tPROTO")
	if len(rules) == 0 {
		fmt.Fprintln(tw, "—\t(no rules)\t\t\t\t\t\t")
	}
	for _, r := range rules {
		size := "auto"
		if r.Source > 0 {
			size = strconv.Itoa(r.Source)
		}
		target := "—"
		if r.Target > 0 {
			target = strconv.Itoa(r.Target)
		}
		project := r.Project
		if project == "" {
			project = "*"
		}
		resolve := r.Resolve
		if resolve == "" {
			resolve = "—"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.Name, project, r.Action, size, target, resolve, r.Proto)
	}
	fmt.Fprintln(tw)

	fmt.Fprintln(tw, "ROUTES")
	fmt.Fprintln(tw, "ID\tHOST\tPROTO\tUPSTREAM\tPORT\tSTATUS\tSERVICE\tPROJECT")
	if len(routes) == 0 {
		fmt.Fprintln(tw, "—\t(no routes)\t\t\t\t\t\t")
	}
	for _, r := range routes {
		port := "—"
		if r.GatewayPort > 0 {
			port = strconv.Itoa(r.GatewayPort)
		}
		upstream := r.TargetAddr
		if upstream == "" {
			upstream = "—"
		}
		host := r.Host
		if r.Collision {
			host += " !"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, host, r.Proto, upstream, port, r.Status, r.Service, r.Project)
	}
	fmt.Fprintln(tw)

	fmt.Fprintln(tw, "LEASES")
	fmt.Fprintln(tw, "ID\tHOST\tPORT\tPID\tCOMMAND")
	if len(leases) == 0 {
		fmt.Fprintln(tw, "—\t(no leases)\t\t")
	}
	for _, l := range leases {
		fmt.Fprintf(tw, "%d\t%s\t%d\t%d\t%s\n", l.ID, l.Host, l.Port, l.PID, truncate(l.Command, 60))
	}
	tw.Flush()

	if len(warnings) > 0 {
		fmt.Println("\nWARNINGS")
		for _, w := range warnings {
			fmt.Println("  ! " + w)
		}
	}
	if !st.DockerOK && st.DockerError != "" {
		fmt.Println("\nDOCKER ERROR")
		fmt.Println("  " + st.DockerError)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func newRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <id>",
		Short: "Remove a rule, route or lease by id (rN, tN, lN, or N)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			_, c, err := clientConfig()
			if err != nil {
				return err
			}
			kind, id, err := resolveTarget(ctx, c, args[0])
			if err != nil {
				return err
			}
			var path string
			switch kind {
			case "rule":
				path = fmt.Sprintf("/v1/rules/%d", id)
			case "route":
				path = fmt.Sprintf("/v1/routes/%d", id)
			case "lease":
				path = fmt.Sprintf("/v1/leases/%d", id)
			}
			var out map[string]any
			if err := c.Delete(ctx, path, &out); err != nil {
				return err
			}
			fmt.Printf("removed %s %d\n", kind, id)
			return nil
		},
	}
}

func resolveTarget(ctx context.Context, c *Client, arg string) (string, int64, error) {
	kind := ""
	num := arg
	if len(arg) > 1 {
		switch arg[0] {
		case 'r':
			kind, num = "rule", arg[1:]
		case 't':
			kind, num = "route", arg[1:]
		case 'l':
			kind, num = "lease", arg[1:]
		}
	}
	id, err := strconv.ParseInt(num, 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("invalid id %q (use rN for rules, tN for routes, lN for leases)", arg)
	}
	if kind != "" {
		return kind, id, nil
	}
	var rules []ruleJSON
	if err := c.Get(ctx, "/v1/rules", &rules); err == nil {
		for _, r := range rules {
			if r.ID == id {
				return "rule", id, nil
			}
		}
	}
	var routes []routeJSON
	if err := c.Get(ctx, "/v1/routes", &routes); err == nil {
		for _, r := range routes {
			if r.ID == id {
				return "route", id, nil
			}
		}
	}
	var leases []leaseJSON
	if err := c.Get(ctx, "/v1/leases", &leases); err == nil {
		for _, l := range leases {
			if l.ID == id {
				return "lease", id, nil
			}
		}
	}
	return "", 0, fmt.Errorf("no rule, route or lease with id %d", id)
}
