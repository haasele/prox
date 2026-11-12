package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/haasele/prox/internal/api"
	"github.com/haasele/prox/internal/hostproc"
	"github.com/haasele/prox/internal/policy"
	"github.com/haasele/prox/internal/registry"
)

func (d *Daemon) CreateLease(ctx context.Context, req api.LeaseRequest) (*api.LeaseResponse, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = filepath.Base(strings.TrimSpace(req.Dir))
	}
	if name == "" || name == "." || name == "/" {
		name = "app"
	}
	name = policy.SanitizeLabel(name)
	port := req.Port
	if port == 0 {
		p, err := hostproc.FreePort()
		if err != nil {
			return nil, err
		}
		port = p
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid port %d", port)
	}
	host := req.Host
	if host == "" {
		host = policy.DefaultHost(name, "", d.Cfg.ProjectDomain)
	} else {
		host = policy.NormalizeResolve(host, d.Cfg.ProjectDomain)
	}
	collision := false
	taken := func(h string) bool {
		if h == policy.DashboardHost {
			return true
		}
		if _, err := d.Reg.GetRouteByHost(h, "http"); err == nil {
			return true
		}
		leases, _ := d.Reg.ListLeases()
		for _, l := range leases {
			if l.Host == h {
				return true
			}
		}
		return false
	}
	if taken(host) {
		host = policy.UniqueHost(host, taken)
		collision = true
	}
	lease := &registry.Lease{
		Name: name, Host: host, PID: req.PID, Port: port,
		Dir: req.Dir, Command: req.Command,
	}
	if err := d.Reg.CreateLease(lease); err != nil {
		return nil, err
	}
	route := &registry.Route{
		Host: host, ContainerID: "lease:" + strconv.FormatInt(lease.ID, 10),
		ContainerName: name, Proto: "http", SourcePort: port, GatewayPort: 443,
		TargetAddr: fmt.Sprintf("127.0.0.1:%d", port), Status: "up",
		Collision: collision, Redirect: true,
	}
	if err := d.Reg.CreateRoute(route); err != nil {
		_, _ = d.Reg.DeleteLease(lease.ID)
		return nil, err
	}
	if collision {
		d.Reg.AddEvent("warn", "collision", fmt.Sprintf("lease host renamed to %s", host), map[string]any{"host": host, "name": name})
	}
	d.Reg.AddEvent("info", "lease", policy.RouteDescription(name, "http", host, port, 443, d.Cfg.HTTPSPort), map[string]any{
		"lease": lease.ID, "host": host, "url": policy.RouteURL("http", host, 443, d.Cfg.HTTPSPort),
		"port": port, "source_port": port, "pid": req.PID, "command": req.Command,
	})
	if req.PID > 0 {
		d.startLeaseWatch(lease.ID, req.PID)
	}
	d.requestSync()
	return &api.LeaseResponse{Lease: *lease, Route: *route}, nil
}

func (d *Daemon) DeleteLease(ctx context.Context, id int64) error {
	lease, err := d.Reg.GetLease(id)
	if err != nil {
		return fmt.Errorf("lease %d not found", id)
	}
	_, _ = d.Reg.DeleteRoutesForContainer("lease:" + strconv.FormatInt(id, 10))
	if _, err := d.Reg.DeleteLease(id); err != nil {
		return err
	}
	d.leaseMu.Lock()
	delete(d.watchedLeases, id)
	d.leaseMu.Unlock()
	d.Reg.AddEvent("info", "lease", fmt.Sprintf("lease %s released", lease.Host), map[string]any{"lease": id, "host": lease.Host})
	d.requestSync()
	return nil
}

func (d *Daemon) reapLeases(ctx context.Context) {
	leases, err := d.Reg.ListLeases()
	if err != nil {
		return
	}
	for _, l := range leases {
		if l.PID <= 0 {
			_ = d.DeleteLease(ctx, l.ID)
			continue
		}
		if !hostproc.Alive(l.PID) {
			_ = d.DeleteLease(ctx, l.ID)
			continue
		}
		d.startLeaseWatch(l.ID, l.PID)
	}
}

func (d *Daemon) startLeaseWatch(id int64, pid int) {
	d.leaseMu.Lock()
	if d.watchedLeases[id] {
		d.leaseMu.Unlock()
		return
	}
	d.watchedLeases[id] = true
	d.leaseMu.Unlock()

	d.watchMu.Lock()
	if d.closing {
		d.watchMu.Unlock()
		return
	}
	d.leaseWG.Add(1)
	d.watchMu.Unlock()

	go func() {
		defer func() {
			d.watchMu.Lock()
			d.leaseWG.Done()
			d.watchMu.Unlock()
		}()
		hostproc.WatchPID(d.ctx, pid)
		if d.ctx.Err() != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = d.DeleteLease(ctx, id)
	}()
}
