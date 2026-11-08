package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/haasele/prox/internal/api"
	"github.com/haasele/prox/internal/dockerclient"
	"github.com/haasele/prox/internal/dockerproxy"
	"github.com/haasele/prox/internal/policy"
	"github.com/haasele/prox/internal/registry"
)

// Reconcile makes the registry and router match Docker after startup, events,
// and API mutations. It is idempotent.
func (d *Daemon) Reconcile(ctx context.Context) error {
	d.reconcileMu.Lock()
	defer d.reconcileMu.Unlock()

	list, err := d.Docker.ListContainers(ctx, true)
	if err != nil {
		return err
	}
	if err := d.Proxy.EnsureNetwork(ctx); err != nil {
		return err
	}
	seen := map[string]bool{}
	var unmanaged []api.UnmanagedView
	changes := 0

	for _, sum := range list {
		seen[sum.ID] = true
		managed := sum.Labels[dockerproxy.LabelManaged] == "true"
		meta, _ := d.Reg.GetContainerMeta(sum.ID)
		if !managed && meta == nil {
			if v, ok := d.unmanagedView(sum); ok {
				unmanaged = append(unmanaged, v)
			}
			continue
		}
		c, err := d.Docker.InspectContainer(ctx, sum.ID)
		if err != nil {
			d.Log.Warn("inspect container", "id", sum.ID, "err", err)
			continue
		}
		if n, err := d.reconcileContainer(ctx, c, meta); err != nil {
			d.Log.Warn("reconcile container", "name", c.Name, "err", err)
		} else {
			changes += n
		}
	}

	// Remove routes and metadata for destroyed containers, with a grace period
	// so a create racing this sweep is not deleted.
	cutoff := time.Now().Add(-90 * time.Second).Unix()
	routes, _ := d.Reg.ListRoutes()
	routesByContainer := map[string][]registry.Route{}
	for _, r := range routes {
		if r.ContainerID == "" {
			continue
		}
		routesByContainer[r.ContainerID] = append(routesByContainer[r.ContainerID], r)
	}
	for id, rs := range routesByContainer {
		if seen[id] || strings.HasPrefix(id, "lease:") {
			continue
		}
		for _, r := range rs {
			if r.CreatedAt >= cutoff {
				continue
			}
			d.deleteRouteRecord(r)
			changes++
		}
	}
	if metas, err := d.Reg.ListContainerMeta(); err == nil {
		for _, m := range metas {
			if seen[m.ID] || m.CreatedAt >= cutoff {
				continue
			}
			_ = d.Reg.DeleteContainerMeta(m.ID)
		}
	}

	d.reconcileGatewayPorts(routes)
	d.reapLeases(ctx)

	d.unmanagedMu.Lock()
	d.unmanaged = unmanaged
	d.unmanagedMu.Unlock()

	if changes > 0 {
		d.Reg.AddEvent("info", "reconcile", fmt.Sprintf("reconciled %d route change(s)", changes), map[string]any{"containers": len(list), "changes": changes})
	}
	d.requestSync()
	return nil
}

func (d *Daemon) reconcileContainer(ctx context.Context, c *dockerclient.Container, meta *registry.ContainerMeta) (int, error) {
	name := strings.TrimPrefix(c.Name, "/")
	labels := c.Config.Labels
	decision := policy.Decision{}
	if meta != nil && len(meta.Decision) > 0 {
		_ = json.Unmarshal(meta.Decision, &decision)
	}
	action := decision.Action
	if action == "" {
		action = labels[dockerproxy.LabelAction]
	}
	if action == policy.ActionPublish || action == policy.ActionWarn || action == policy.ActionSkip || action == "" {
		return 0, nil
	}
	proto := decision.Proto
	if proto == "" {
		proto = labels[dockerproxy.LabelProto]
	}
	source := decision.SourcePort
	if source == 0 {
		source, _ = strconv.Atoi(labels[dockerproxy.LabelPort])
	}
	if source == 0 {
		source = firstExposedPort(c)
	}
	host := decision.Host
	if host == "" {
		host = labels[dockerproxy.LabelHost]
	}
	if host == "" {
		host = policy.DefaultHost(labels["com.docker.compose.service"], labels["com.docker.compose.project"], d.Cfg.ProjectDomain)
	}
	if proto == "" {
		return 0, nil
	}
	target, _ := strconv.Atoi(labels[dockerproxy.LabelTarget])
	if target == 0 {
		target = decision.TargetPort
	}
	redirect := true
	if v := labels[dockerproxy.LabelRedirect]; v != "" {
		redirect, _ = strconv.ParseBool(v)
	}
	ruleID, _ := strconv.ParseInt(labels[dockerproxy.LabelRule], 10, 64)

	routes, err := d.Reg.RoutesForContainer(c.ID)
	if err != nil {
		return 0, err
	}
	if len(routes) == 0 {
		host, collision := d.claimHost(host, c.ID, name, proto)
		if proto == "tcp" && target != 443 && target == 0 {
			target = 0
		}
		if proto == "tcp" && target != 443 {
			key := d.gatewayKey(name, source)
			port, err := d.Alloc.Allocate(target, key)
			if err != nil {
				return 0, err
			}
			target = port
		}
		r := &registry.Route{
			Host: host, ContainerID: c.ID, ContainerName: name,
			Project: labels["com.docker.compose.project"], Service: labels["com.docker.compose.service"],
			Image: c.Config.Image, Proto: proto, SourcePort: source, GatewayPort: target,
			RuleID: ruleID, Status: "pending", Collision: collision, Redirect: redirect,
		}
		if err := d.Reg.CreateRoute(r); err != nil {
			return 0, err
		}
		if collision {
			d.Reg.AddEvent("warn", "collision", fmt.Sprintf("host collision: %s routed as %s", host, r.Host), map[string]any{
				"host": r.Host, "container": name, "project": r.Project,
			})
		}
		routes = []registry.Route{*r}
	}
	changes := 0
	for _, r := range routes {
		if r.SourcePort == 0 && c.State.Running {
			if p := detectProbePort(r.Proto, c.State.Pid); p > 0 {
				_ = d.Reg.UpdateRouteSource(r.ID, p)
				d.Reg.AddEvent("info", "route", fmt.Sprintf("detected listening port %d for %s", p, r.Host), map[string]any{
					"host": r.Host, "port": p, "container": name,
				})
				r.SourcePort = p
				changes++
			}
		}
		if r.SourcePort == 0 {
			if c.State.Running {
				_ = d.Reg.UpdateRouteStatus(r.ID, "down", "")
			}
			continue
		}
		status, addr := "down", ""
		switch {
		case !c.State.Running && c.State.Status == "created":
			status = "pending"
		case !c.State.Running:
			status = "down"
		case c.State.Health != nil && c.State.Health.Status == "unhealthy":
			status = "down"
		default:
			addr, err = d.Backend.ResolveAddress(c, r.SourcePort)
			if err != nil && d.Backend.Name() == "native" {
				_ = d.Docker.ConnectNetwork(ctx, d.Proxy.Network, c.ID)
				if c2, ierr := d.Docker.InspectContainer(ctx, c.ID); ierr == nil {
					c = c2
					addr, err = d.Backend.ResolveAddress(c, r.SourcePort)
				}
			}
			if err != nil || addr == "" {
				status = "down"
			} else {
				status = "up"
			}
		}
		if r.Status != status || (status == "up" && r.TargetAddr != addr) {
			_ = d.Reg.UpdateRouteStatus(r.ID, status, addr)
			changes++
		}
	}
	return changes, nil
}

func (d *Daemon) claimHost(proposed, containerID, containerName, proto string) (string, bool) {
	if proposed == "" {
		proposed = policy.DefaultHost(containerName, "", d.Cfg.ProjectDomain)
	}
	reserved := false
	if (proto == "http" || proto == "sni") && proposed == policy.DashboardHost {
		proposed = policy.UniqueHost(proposed, func(h string) bool { return h == policy.DashboardHost })
		reserved = true
	}
	if r, err := d.Reg.GetRouteByHost(proposed, proto); err == nil {
		if r.ContainerID == containerID || r.ContainerName == containerName {
			return proposed, r.Collision
		}
		taken := func(h string) bool {
			_, err := d.Reg.GetRouteByHost(h, proto)
			return err == nil
		}
		return policy.UniqueHost(proposed, taken), true
	}
	return proposed, reserved
}

func firstExposedPort(c *dockerclient.Container) int {
	best := 0
	for key := range c.Config.ExposedPorts {
		p, proto := splitPortKey(key)
		if proto != "tcp" || p == 0 {
			continue
		}
		if best == 0 || p < best {
			best = p
		}
	}
	return best
}

func splitPortKey(s string) (int, string) {
	proto := "tcp"
	if i := strings.Index(s, "/"); i >= 0 {
		proto = s[i+1:]
		s = s[:i]
	}
	p, _ := strconv.Atoi(s)
	return p, proto
}

func (d *Daemon) gatewayKey(containerName string, source int) string {
	return "name:" + containerName + ":" + strconv.Itoa(source)
}

func (d *Daemon) reconcileGatewayPorts(routes []registry.Route) {
	inUse := map[string]bool{}
	for _, r := range routes {
		if r.Proto != "tcp" || r.GatewayPort == 0 || r.GatewayPort == 443 {
			continue
		}
		key := d.gatewayKey(r.ContainerName, r.SourcePort)
		inUse[key] = true
		if err := d.Reg.ReserveGatewayPort(r.GatewayPort, key); err != nil {
			// Someone else owns the number; allocate a fresh one.
			port, aerr := d.Alloc.Allocate(0, key)
			if aerr != nil {
				d.Log.Error("gateway reallocate", "host", r.Host, "err", aerr)
				continue
			}
			if _, uerr := d.Reg.DB.Exec(`UPDATE routes SET gateway_port=?, updated_at=? WHERE id=?`, port, time.Now().Unix(), r.ID); uerr != nil {
				d.Log.Error("update gateway port", "err", uerr)
				continue
			}
			inUse[key] = true
			d.Reg.AddEvent("warn", "route", fmt.Sprintf("gateway port for %s reassigned to %d", r.Host, port), nil)
		}
	}
	_ = d.Reg.ReleaseUnusedGatewayPortsBefore(inUse, time.Now().Add(-2*time.Minute).Unix())
}

func (d *Daemon) deleteRouteRecord(r registry.Route) {
	if r.Proto == "tcp" && r.GatewayPort != 0 && r.GatewayPort != 443 {
		d.Alloc.Release(d.gatewayKey(r.ContainerName, r.SourcePort))
	}
	_, _ = d.Reg.DeleteRoutesForContainer(r.ContainerID)
}

func (d *Daemon) unmanagedView(sum dockerclient.ContainerSummary) (api.UnmanagedView, bool) {
	var ports []string
	hasHost := false
	for _, p := range sum.Ports {
		if p.PublicPort > 0 {
			hasHost = true
			ports = append(ports, fmt.Sprintf("%s:%d->%d/%s", p.IP, p.PublicPort, p.PrivatePort, p.Type))
		}
	}
	if !hasHost {
		return api.UnmanagedView{}, false
	}
	name := strings.TrimPrefix(firstName(sum.Names), "/")
	project := sum.Labels["com.docker.compose.project"]
	service := sum.Labels["com.docker.compose.service"]
	source := 0
	for _, p := range sum.Ports {
		if p.PublicPort > 0 {
			source = p.PrivatePort
			break
		}
	}
	proto := "http"
	if profile, ok := d.Engine.ProfileForImage(sum.Image); ok {
		proto = profile.Proto
		if source == 0 {
			source = profile.Port
		}
	}
	if source == 0 && len(sum.Ports) > 0 {
		source = sum.Ports[0].PrivatePort
	}
	suggestName := service
	if suggestName == "" {
		suggestName = name
	}
	return api.UnmanagedView{
		ID: sum.ID, Name: name, Image: sum.Image, Project: project, Service: service,
		Ports: ports, Labels: sum.Labels,
		Suggestion: api.Suggestion{
			Name: suggestName, Project: project, Source: source, Proto: proto,
			Resolve: policy.NormalizeResolve(suggestName, d.Cfg.ProjectDomain), Action: "allow",
		},
	}, true
}

func firstName(names []string) string {
	if len(names) > 0 {
		return names[0]
	}
	return ""
}

func (d *Daemon) Containers(ctx context.Context) (api.ContainersView, error) {
	routes, err := d.Reg.ListRoutes()
	if err != nil {
		return api.ContainersView{}, err
	}
	var view api.ContainersView
	for _, r := range routes {
		mv := api.ManagedView{Route: r}
		if r.Proto == "http" {
			mv.URL = "https://" + d.Cfg.HTTPSHostPort(r.Host)
		} else if r.Proto == "tcp" {
			mv.URL = fmt.Sprintf("tcp://%s:%d", r.Host, r.GatewayPort)
		}
		view.Managed = append(view.Managed, mv)
		if r.Collision {
			view.Warnings = append(view.Warnings, fmt.Sprintf("host collision: %s is routed as %s instead of its short name", r.Service, r.Host))
		}
		if r.Status == "down" {
			view.Warnings = append(view.Warnings, fmt.Sprintf("route %s is down (container stopped or unhealthy)", r.Host))
		}
		if r.Status == "pending" {
			view.Warnings = append(view.Warnings, fmt.Sprintf("route %s is pending (container not started)", r.Host))
		}
	}
	d.unmanagedMu.RLock()
	view.Unmanaged = append(view.Unmanaged, d.unmanaged...)
	d.unmanagedMu.RUnlock()
	for _, u := range view.Unmanaged {
		view.Warnings = append(view.Warnings, fmt.Sprintf("container %s publishes host ports directly and is not proxied: %s", u.Name, strings.Join(u.Ports, ", ")))
	}
	if errStr := d.getDockerError(); errStr != "" {
		view.Warnings = append(view.Warnings, "Docker is unreachable: "+errStr)
	}
	if v, _ := d.Reg.GetMeta("enforcement"); v == "on" {
		if st := d.Authz.Status(); !st.Running {
			view.Warnings = append(view.Warnings, "enforcement is enabled but the AuthZ plugin is not running: "+st.Error)
		}
	}
	if view.Managed == nil {
		view.Managed = []api.ManagedView{}
	}
	if view.Unmanaged == nil {
		view.Unmanaged = []api.UnmanagedView{}
	}
	if view.Warnings == nil {
		view.Warnings = []string{}
	}
	return view, nil
}

func (d *Daemon) DeleteRoute(ctx context.Context, id int64) error {
	route, err := d.Reg.GetRoute(id)
	if err != nil {
		return fmt.Errorf("route %d not found", id)
	}
	if route.Proto == "tcp" && route.GatewayPort != 0 && route.GatewayPort != 443 {
		d.Alloc.Release(d.gatewayKey(route.ContainerName, route.SourcePort))
	}
	if route.ContainerID != "" && !strings.HasPrefix(route.ContainerID, "lease:") && route.ContainerID != "0" {
		if meta, merr := d.Reg.GetContainerMeta(route.ContainerID); merr == nil {
			var dec policy.Decision
			if json.Unmarshal(meta.Decision, &dec) == nil {
				dec.Action = policy.ActionSkip
				dec.Reason = "route removed with gw rm"
				if b, jerr := json.Marshal(dec); jerr == nil {
					meta.Decision = b
					_ = d.Reg.SaveContainerMeta(meta)
				}
			}
		}
	}
	if _, err := d.Reg.DeleteRoute(id); err != nil {
		return err
	}
	if strings.HasPrefix(route.ContainerID, "lease:") {
		var leaseID int64
		fmt.Sscanf(strings.TrimPrefix(route.ContainerID, "lease:"), "%d", &leaseID)
		if leaseID > 0 {
			_, _ = d.Reg.DeleteLease(leaseID)
		}
	}
	d.Reg.AddEvent("info", "route", fmt.Sprintf("route %d (%s) removed", id, route.Host), map[string]any{"id": id, "host": route.Host})
	d.requestSync()
	return nil
}
