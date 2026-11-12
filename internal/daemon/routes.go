package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"strings"

	"github.com/haasele/prox/internal/api"
	"github.com/haasele/prox/internal/policy"
	"github.com/haasele/prox/internal/registry"
)

// CreateManualRoute makes an existing listener reachable under a prox hostname.
// It is used by the dashboard for containers that were created outside prox:
// the rule covers future creates, this route covers the running process.
func (d *Daemon) CreateManualRoute(ctx context.Context, req api.RouteRequest) (*registry.Route, error) {
	host := policy.NormalizeResolve(req.Host, d.Cfg.ProjectDomain)
	if host == "" {
		return nil, fmt.Errorf("host is required")
	}
	if host == policy.DashboardHost {
		return nil, fmt.Errorf("%s is reserved for the prox dashboard", host)
	}
	if req.TargetAddr == "" {
		return nil, fmt.Errorf("target_addr is required")
	}
	if _, _, err := net.SplitHostPort(req.TargetAddr); err != nil {
		return nil, fmt.Errorf("target_addr must be host:port: %w", err)
	}
	proto := strings.ToLower(req.Proto)
	if proto == "" {
		proto = "http"
	}
	if proto != "http" {
		return nil, fmt.Errorf("manual routes currently support http only")
	}
	if req.SourcePort < 0 || req.SourcePort > 65535 {
		return nil, fmt.Errorf("source_port must be between 1 and 65535")
	}
	if _, err := d.Reg.GetRouteByHost(host, proto); err == nil {
		return nil, fmt.Errorf("host %s is already routed", host)
	}
	redirect := true
	if req.Redirect != nil {
		redirect = *req.Redirect
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = strings.Split(host, ".")[0]
	}
	route := &registry.Route{
		Host:          host,
		ContainerID:   "manual:" + randomID(),
		ContainerName: name,
		Service:       name,
		Proto:         proto,
		SourcePort:    req.SourcePort,
		GatewayPort:   443,
		TargetAddr:    req.TargetAddr,
		Status:        "up",
		Redirect:      redirect,
	}
	if err := d.Reg.CreateRoute(route); err != nil {
		return nil, err
	}
	d.Reg.AddEvent("info", "route", fmt.Sprintf("manual route %s -> %s", host, req.TargetAddr), map[string]any{
		"route": route.ID, "host": host, "target": req.TargetAddr, "source_port": req.SourcePort, "redirect": redirect,
	})
	d.requestSync()
	return route, nil
}

func randomID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", 0)
	}
	return hex.EncodeToString(b[:])
}
