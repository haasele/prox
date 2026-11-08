package cli

import (
	"context"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
)

type ruleJSON struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Project  string `json:"project"`
	Source   int    `json:"source"`
	Target   int    `json:"target"`
	Resolve  string `json:"resolve"`
	Proto    string `json:"proto"`
	Action   string `json:"action"`
	Redirect bool   `json:"redirect"`
}

type portAliases struct {
	source, container       int
	sourceSet, containerSet bool
	target, gateway         int
	targetSet, gatewaySet   bool
}

func normalizePortAliases(p portAliases) (source, target int, err error) {
	if p.sourceSet && p.containerSet {
		return 0, 0, fmt.Errorf("--source and --container-port are aliases; use only one of them")
	}
	if p.targetSet && p.gatewaySet {
		return 0, 0, fmt.Errorf("--target and --gateway-port are aliases; use only one of them")
	}
	source, target = p.source, p.target
	if p.containerSet {
		source = p.container
	}
	if p.gatewaySet {
		target = p.gateway
	}
	return source, target, nil
}

func newAllowCmd() *cobra.Command {
	var (
		source, container       int
		target, gateway         int
		resolve, project, proto string
		publish, noRedirect     bool
	)
	cmd := &cobra.Command{
		Use:   "allow <name>",
		Short: "Allow and route a service, image, or profile",
		Example: `  gw allow postgres --source 5432 --target 5432 --resolve database
  gw allow postgres                                  # profile defaults
  gw allow web --container-port 3000                 # alias for --source
  gw allow web --publish                             # keep the host port binding`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			src, tgt, err := normalizePortAliases(portAliases{
				source: source, container: container,
				sourceSet: cmd.Flags().Changed("source"), containerSet: cmd.Flags().Changed("container-port"),
				target: target, gateway: gateway,
				targetSet: cmd.Flags().Changed("target"), gatewaySet: cmd.Flags().Changed("gateway-port"),
			})
			if err != nil {
				return err
			}
			action := "allow"
			if publish {
				action = "publish"
			}
			body := map[string]any{
				"name": args[0], "project": project, "source": src, "target": tgt,
				"resolve": resolve, "proto": proto, "action": action,
			}
			if cmd.Flags().Changed("no-redirect") {
				body["redirect"] = !noRedirect
			}
			_, c, err := clientConfig()
			if err != nil {
				return err
			}
			var rule ruleJSON
			if err := c.Post(context.Background(), "/v1/rules", body, &rule); err != nil {
				return err
			}
			fmt.Println(describeRule(rule))
			return nil
		},
	}
	cmd.Flags().IntVar(&source, "source", 0, "container port to route")
	cmd.Flags().IntVar(&container, "container-port", 0, "alias for --source")
	cmd.Flags().IntVar(&target, "target", 0, "gateway port (default 443 for HTTP, source for TCP)")
	cmd.Flags().IntVar(&gateway, "gateway-port", 0, "alias for --target")
	cmd.Flags().StringVar(&resolve, "resolve", "", "hostname (default <name>.localhost)")
	cmd.Flags().StringVar(&project, "project", "", "only match this Compose project")
	cmd.Flags().StringVar(&proto, "proto", "", "http or tcp (default from profile)")
	cmd.Flags().BoolVar(&publish, "publish", false, "keep host port bindings (bypass the gateway)")
	cmd.Flags().BoolVar(&noRedirect, "no-redirect", false, "serve plain HTTP for this host instead of redirecting")
	return cmd
}

func newDenyCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "deny <name>",
		Short: "Deny containers matching a service, image, or profile",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, c, err := clientConfig()
			if err != nil {
				return err
			}
			var rule ruleJSON
			if err := c.Post(context.Background(), "/v1/rules", map[string]any{
				"name": args[0], "project": project, "action": "deny",
			}, &rule); err != nil {
				return err
			}
			fmt.Println(describeRule(rule))
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "only match this Compose project")
	return cmd
}

func describeRule(r ruleJSON) string {
	port := ""
	if r.Source > 0 {
		port = " (container port " + strconv.Itoa(r.Source) + ")"
	}
	switch r.Action {
	case "route-http":
		return fmt.Sprintf("rule %d: %s → https://%s%s", r.ID, r.Name, r.Resolve, port)
	case "route-tcp":
		if r.Target == 443 {
			return fmt.Sprintf("rule %d: %s → %s:443 via SNI%s", r.ID, r.Name, r.Resolve, port)
		}
		target := "auto"
		if r.Target > 0 {
			target = strconv.Itoa(r.Target)
		}
		return fmt.Sprintf("rule %d: %s → %s:%s (tcp gateway)%s", r.ID, r.Name, r.Resolve, target, port)
	case "publish":
		return fmt.Sprintf("rule %d: %s keeps its host port bindings", r.ID, r.Name)
	case "deny":
		s := fmt.Sprintf("rule %d: %s is denied", r.ID, r.Name)
		if r.Project != "" {
			s += " in project " + r.Project
		}
		return s
	}
	return fmt.Sprintf("rule %d: %s (%s)", r.ID, r.Name, r.Action)
}
