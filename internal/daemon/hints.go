package daemon

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	"github.com/haasele/prox/internal/hints"
	"github.com/haasele/prox/internal/policy"
	"github.com/haasele/prox/internal/tcpforward"
)

// applyHints keeps the original host ports of HTTP routes answered by a prox
// redirect page. It is best-effort: a port owned by another process is skipped
// and reported once as an event.
func (d *Daemon) applyHints() {
	if d.Hints == nil {
		return
	}
	var desired []hints.Hint
	if d.Cfg.OriginalHints || len(d.Cfg.OwnHTTPPorts) > 0 {
		desired = d.desiredHints()
	}
	for _, ch := range d.Hints.Reconcile(desired) {
		switch {
		case ch.Started && ch.URL == "":
			d.Reg.AddEvent("info", "hint", fmt.Sprintf("original port %d reserved by prox; waiting for an application", ch.Port), map[string]any{"port": ch.Port})
		case ch.Started:
			d.Reg.AddEvent("info", "hint", fmt.Sprintf("original port %d now answers with a prox redirect -> %s", ch.Port, ch.URL), map[string]any{
				"port": ch.Port, "url": ch.URL,
			})
		case ch.Updated && ch.URL == "":
			d.Reg.AddEvent("info", "hint", fmt.Sprintf("original port %d is waiting for an application", ch.Port), map[string]any{"port": ch.Port})
		case ch.Updated:
			d.Reg.AddEvent("info", "hint", fmt.Sprintf("original port %d now redirects to %s", ch.Port, ch.URL), map[string]any{
				"port": ch.Port, "url": ch.URL,
			})
		case ch.Stopped:
			d.Reg.AddEvent("info", "hint", fmt.Sprintf("original port %d released", ch.Port), map[string]any{"port": ch.Port})
		case ch.Err != nil:
			d.Reg.AddEvent("warn", "hint", fmt.Sprintf("cannot answer on original port %d: %v", ch.Port, ch.Err), map[string]any{
				"port": ch.Port, "error": ch.Err.Error(),
			})
		}
	}
}

func (d *Daemon) applyTCPOwned() {
	if d.OwnedTCP == nil {
		return
	}
	var desired []tcpforward.Target
	if len(d.Cfg.OwnTCPPorts) > 0 {
		desired = d.desiredTCPOwned()
	}
	for _, ch := range d.OwnedTCP.Reconcile(desired) {
		var msg string
		switch {
		case ch.Stopped:
			msg = fmt.Sprintf("owned TCP port %d released", ch.Port)
		case ch.Started && ch.Target != "":
			msg = fmt.Sprintf("owned TCP port %d reserved and forwarding to %s", ch.Port, ch.Target)
		case ch.Started:
			msg = fmt.Sprintf("owned TCP port %d reserved; waiting for an application", ch.Port)
		case ch.Updated && ch.Target != "":
			msg = fmt.Sprintf("owned TCP port %d now forwards to %s", ch.Port, ch.Target)
		case ch.Updated:
			msg = fmt.Sprintf("owned TCP port %d has no application target", ch.Port)
		case ch.Err != nil:
			msg = fmt.Sprintf("cannot own TCP port %d: %v", ch.Port, ch.Err)
		default:
			continue
		}
		level := "info"
		if ch.Err != nil {
			level = "warn"
		}
		d.Reg.AddEvent(level, "port", msg, map[string]any{
			"port": ch.Port, "target": ch.Target, "error": errorString(ch.Err),
		})
	}
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (d *Daemon) desiredHints() []hints.Hint {
	routes, err := d.Reg.ListRoutes()
	if err != nil {
		return nil
	}
	own := map[int]bool{}
	for _, port := range d.Cfg.OwnHTTPPorts {
		own[port] = true
	}
	type owner struct {
		routeID int64
		hint    hints.Hint
	}
	byPort := map[int]owner{}
	for port := range own {
		if d.hintPortReserved(port) {
			continue
		}
		byPort[port] = owner{hint: hints.Hint{Port: port, Name: "prox"}}
	}
	for _, r := range routes {
		if r.Proto != "http" && r.Proto != "sni" {
			continue
		}
		meta, err := d.Reg.GetContainerMeta(r.ContainerID)
		if err != nil {
			// Leases and routes without stored create metadata have no original
			// host port to answer on.
			continue
		}
		name := r.Service
		if name == "" {
			name = r.ContainerName
		}
		if name == "" {
			name = r.Host
		}
		url := policy.RouteURL(r.Proto, r.Host, r.GatewayPort, d.Cfg.HTTPSPort)
		for _, port := range originalHostPorts(meta.Original) {
			if (!d.Cfg.OriginalHints && !own[port]) || d.hintPortReserved(port) {
				continue
			}
			if cur, ok := byPort[port]; ok && cur.routeID > 0 && cur.routeID <= r.ID {
				continue
			}
			byPort[port] = owner{routeID: r.ID, hint: hints.Hint{Port: port, Name: name, URL: url}}
		}
	}
	out := make([]hints.Hint, 0, len(byPort))
	for _, o := range byPort {
		out = append(out, o.hint)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}

func (d *Daemon) desiredTCPOwned() []tcpforward.Target {
	own := map[int]bool{}
	for _, port := range d.Cfg.OwnTCPPorts {
		own[port] = true
	}
	if len(own) == 0 {
		return nil
	}
	routes, err := d.Reg.ListRoutes()
	if err != nil {
		return nil
	}
	type owner struct {
		routeID int64
		target  string
	}
	byPort := map[int]owner{}
	for port := range own {
		if d.portReserved(port) || d.ownHTTP(port) {
			continue
		}
		byPort[port] = owner{}
	}
	for _, r := range routes {
		if r.Proto != "tcp" {
			continue
		}
		meta, err := d.Reg.GetContainerMeta(r.ContainerID)
		if err != nil {
			continue
		}
		target := ""
		if r.Status == "up" && r.TargetAddr != "" {
			target = r.TargetAddr
		}
		for _, port := range originalHostPorts(meta.Original) {
			if !own[port] || d.portReserved(port) || d.ownHTTP(port) {
				continue
			}
			if cur, ok := byPort[port]; ok && cur.routeID > 0 && cur.routeID <= r.ID {
				continue
			}
			byPort[port] = owner{routeID: r.ID, target: target}
		}
	}
	out := make([]tcpforward.Target, 0, len(byPort))
	for port, o := range byPort {
		out = append(out, tcpforward.Target{Port: port, Target: o.target})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}

func (d *Daemon) ownHTTP(port int) bool {
	for _, p := range d.Cfg.OwnHTTPPorts {
		if p == port {
			return true
		}
	}
	return false
}

func (d *Daemon) portReserved(port int) bool {
	switch port {
	case 0, d.Cfg.HTTPPort, d.Cfg.HTTPSPort, d.Cfg.DNSPort, d.Cfg.TLSLoopbackPort:
		return true
	}
	return false
}

func (d *Daemon) hintPortReserved(port int) bool {
	if d.portReserved(port) {
		return true
	}
	for _, p := range d.Cfg.OwnTCPPorts {
		if p == port {
			return true
		}
	}
	return false
}

func originalHostPorts(raw []byte) []int {
	if len(raw) == 0 {
		return nil
	}
	var body struct {
		HostConfig struct {
			PortBindings map[string][]struct {
				HostPort string `json:"HostPort"`
			} `json:"PortBindings"`
		} `json:"HostConfig"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return nil
	}
	seen := map[int]bool{}
	var ports []int
	for _, bindings := range body.HostConfig.PortBindings {
		for _, binding := range bindings {
			port, err := strconv.Atoi(binding.HostPort)
			if err != nil || port <= 0 || seen[port] {
				continue
			}
			seen[port] = true
			ports = append(ports, port)
		}
	}
	sort.Ints(ports)
	return ports
}
