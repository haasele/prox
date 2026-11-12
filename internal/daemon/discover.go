package daemon

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/haasele/prox/internal/api"
	"github.com/haasele/prox/internal/discover"
	"github.com/haasele/prox/internal/policy"
)

// discoverLoop finds already-running HTTP dev servers and leases them a stable
// hostname. It never binds their ports: the process keeps its own listener.
func (d *Daemon) discoverLoop() {
	defer d.wg.Done()
	d.discoverOnce()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			d.discoverOnce()
		}
	}
}

func (d *Daemon) discoverOnce() {
	if !d.Cfg.Discover {
		return
	}
	leases, err := d.Reg.ListLeases()
	if err != nil {
		return
	}
	known := map[int]bool{}
	for _, l := range leases {
		known[l.Port] = true
	}
	for _, l := range discover.ScanSkip(os.Getpid(), known) {
		if known[l.Port] || d.hintPortReserved(l.Port) {
			continue
		}
		if !d.Cfg.DiscoverExisting && l.StartTicks > 0 && l.StartTicks < d.procStart {
			continue
		}
		key := fmt.Sprintf("%d:%d", l.PID, l.Port)
		if !d.discoveryAllowed(key) {
			continue
		}
		resp, err := d.CreateLease(context.Background(), api.LeaseRequest{
			Name: l.Name, Port: l.Port, PID: l.PID, Dir: l.Dir, Command: l.Command,
		})
		if err != nil {
			d.Log.Debug("discovered listener not leased", "port", l.Port, "pid", l.PID, "err", err)
			continue
		}
		url := policy.RouteURL(resp.Route.Proto, resp.Route.Host, resp.Route.GatewayPort, d.Cfg.HTTPSPort)
		d.Reg.AddEvent("info", "discover", fmt.Sprintf("found %s on :%d; routed by prox -> %s", l.Name, l.Port, url), map[string]any{
			"name": l.Name, "port": l.Port, "pid": l.PID, "dir": l.Dir, "exec": l.Exec, "command": l.Command,
			"url": url, "route": resp.Route.ID,
		})
		d.Log.Info("discovered listener", "name", l.Name, "port", l.Port, "pid", l.PID, "url", url)
	}
}

// discoveryAllowed rate-limits retries for a PID/port pair that cannot be
// leased yet (for example, a name collision or a process that just exited).
func (d *Daemon) discoveryAllowed(key string) bool {
	d.discoverMu.Lock()
	defer d.discoverMu.Unlock()
	if d.discoverTried == nil {
		d.discoverTried = map[string]time.Time{}
	}
	if t, ok := d.discoverTried[key]; ok && time.Since(t) < 30*time.Second {
		return false
	}
	d.discoverTried[key] = time.Now()
	if len(d.discoverTried) > 512 {
		cutoff := time.Now().Add(-time.Minute)
		for k, t := range d.discoverTried {
			if t.Before(cutoff) {
				delete(d.discoverTried, k)
			}
		}
	}
	return true
}
