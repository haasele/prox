package composecheck

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/haasele/prox/internal/policy"
	"github.com/haasele/prox/internal/registry"
)

type Outcome struct {
	Service     string `json:"service"`
	Image       string `json:"image,omitempty"`
	Project     string `json:"project"`
	Action      string `json:"action"`
	Status      string `json:"status"`
	Host        string `json:"host,omitempty"`
	Proto       string `json:"proto,omitempty"`
	SourcePort  int    `json:"source_port,omitempty"`
	GatewayPort int    `json:"gateway_port,omitempty"`
	RuleID      int64  `json:"rule_id,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Collision   bool   `json:"collision,omitempty"`
	Preferred   int    `json:"preferred_gateway_port,omitempty"`
	GatewayBusy bool   `json:"gateway_port_busy,omitempty"`
}

// PortFree is used to report whether a predicted gateway port is available.
type PortFree func(port int) bool

func Check(eng *policy.Engine, reg *registry.Registry, yamlBytes []byte, filename, projectName string, portFree PortFree) ([]Outcome, error) {
	if filename == "" {
		filename = "compose.yml"
	}
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	configDir := filepath.Dir(filename)
	if abs, err := filepath.Abs(configDir); err == nil {
		wd = abs
	}
	env := types.Mapping{}
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			env[kv[:i]] = kv[i+1:]
		}
	}
	details := types.ConfigDetails{
		WorkingDir:  wd,
		Environment: env,
		ConfigFiles: []types.ConfigFile{{Filename: filepath.Base(filename), Content: yamlBytes}},
	}
	opts := []func(*loader.Options){
		loader.WithProfiles([]string{"*"}),
		func(o *loader.Options) {
			if projectName != "" {
				o.SetProjectName(projectName, true)
			}
		},
	}
	project, err := loader.LoadWithContext(context.Background(), details, opts...)
	if err != nil {
		return nil, fmt.Errorf("parse compose file: %w", err)
	}

	planned := map[string]Outcome{}
	var out []Outcome
	for _, name := range project.ServiceNames() {
		svc := project.Services[name]
		image := svc.Image
		if image == "" && svc.Build != nil {
			image = project.Name + "-" + name
		}
		bindings := map[int][]policy.PortBinding{}
		exposed := []int{}
		for _, p := range svc.Ports {
			port := int(p.Target)
			if port <= 0 {
				continue
			}
			proto := p.Protocol
			if proto == "" {
				proto = "tcp"
			}
			if proto != "tcp" {
				continue
			}
			bindings[port] = append(bindings[port], policy.PortBinding{HostIP: p.HostIP, HostPort: p.Published})
			exposed = append(exposed, port)
		}
		for _, e := range svc.Expose {
			if p, err := strconv.Atoi(string(e)); err == nil && p > 0 {
				exposed = append(exposed, p)
			}
		}
		labels := map[string]string{}
		for k, v := range svc.Labels {
			labels[k] = v
		}
		info := policy.CreateInfo{
			Name:         project.Name + "-" + name + "-1",
			Project:      project.Name,
			Service:      name,
			Image:        image,
			Labels:       labels,
			Bindings:     bindings,
			ExposedPorts: exposed,
			NetworkMode:  svc.NetworkMode,
		}
		decision, err := eng.Decide(info)
		if err != nil {
			return nil, fmt.Errorf("service %s: %w", name, err)
		}
		oc := Outcome{
			Service:     name,
			Image:       image,
			Project:     project.Name,
			Action:      decision.Action,
			Status:      statusFor(decision.Action),
			Host:        decision.Host,
			Proto:       decision.Proto,
			SourcePort:  decision.SourcePort,
			GatewayPort: decision.TargetPort,
			Preferred:   decision.PreferredGatewayPort,
			RuleID:      decision.RuleID,
			Reason:      decision.Reason,
		}
		if oc.Action == policy.ActionRouteHTTP {
			oc.GatewayPort = 443
		}
		if oc.GatewayPort != 0 && oc.GatewayPort != 443 && portFree != nil {
			oc.GatewayBusy = !portFree(oc.GatewayPort)
		}
		if oc.Host != "" {
			key := oc.Host + "|" + oc.Proto
			if prev, ok := planned[key]; ok && prev.Service != name {
				collision := policy.CollisionHost(oc.Host, project.Name, eng.Domain)
				taken := func(h string) bool {
					_, ok := planned[h+"|"+oc.Proto]
					return ok
				}
				oc.Host = policy.UniqueHost(collision, taken)
				oc.Collision = true
			} else if existing, err := reg.GetRouteByHost(oc.Host, oc.Proto); err == nil && existing.Project != "" && !strings.EqualFold(existing.Project, project.Name) {
				oc.Host = policy.CollisionHost(oc.Host, project.Name, eng.Domain)
				oc.Collision = true
			}
			planned[oc.Host+"|"+oc.Proto] = oc
		}
		out = append(out, oc)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out, nil
}

func statusFor(action string) string {
	switch action {
	case policy.ActionRouteHTTP:
		return "routed-http"
	case policy.ActionRouteTCP:
		return "routed-tcp"
	case policy.ActionPublish:
		return "published"
	case policy.ActionDeny:
		return "denied"
	case policy.ActionWarn:
		return "left-published"
	case policy.ActionSkip:
		return "untouched"
	}
	return action
}
