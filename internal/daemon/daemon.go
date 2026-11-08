package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/haasele/prox/internal/api"
	"github.com/haasele/prox/internal/authz"
	"github.com/haasele/prox/internal/backend"
	"github.com/haasele/prox/internal/config"
	"github.com/haasele/prox/internal/dns"
	"github.com/haasele/prox/internal/dockerclient"
	"github.com/haasele/prox/internal/dockerproxy"
	"github.com/haasele/prox/internal/policy"
	"github.com/haasele/prox/internal/registry"
	"github.com/haasele/prox/internal/router"
	"github.com/haasele/prox/profiles"
)

type Daemon struct {
	Cfg     *config.Config
	Log     *slog.Logger
	Reg     *registry.Registry
	Engine  *policy.Engine
	Docker  *dockerclient.Client
	Backend backend.Backend
	Proxy   *dockerproxy.Proxy
	Alloc   *dockerproxy.Allocator
	Router  *router.Caddy
	DNS     *dns.Server
	API     *api.API
	Authz   *authz.Server

	version  string
	dashDial string

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	reconcileMu   sync.Mutex
	syncCh        chan struct{}
	reconcileCh   chan struct{}
	leaseMu       sync.Mutex
	watchedLeases map[int64]bool
	leaseWG       sync.WaitGroup
	watchMu       sync.Mutex
	closing       bool
	unmanagedMu   sync.RWMutex
	unmanaged     []api.UnmanagedView
	dockerErrMu   sync.RWMutex
	dockerErr     string

	closeOnce sync.Once
}

func New(ctx context.Context, cfg *config.Config, version string) (*Daemon, error) {
	if err := cfg.EnsureDirs(); err != nil {
		return nil, err
	}
	logger := newLogger(cfg.LogLevel)
	reg, err := registry.Open(cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("open registry: %w", err)
	}
	set, err := profiles.LoadBuiltins()
	if err != nil {
		reg.Close()
		return nil, err
	}
	if merged, err := set.LoadUserDir(cfg.ProfilesDir); err != nil {
		logger.Warn("user profiles ignored", "dir", cfg.ProfilesDir, "err", err)
	} else {
		set = merged
	}
	eng := policy.New(reg, set, cfg.ProjectDomain)

	rules, _ := reg.ListRules()
	if len(rules) == 0 {
		if _, err := os.Stat(cfg.RulesFile); err == nil {
			if n, err := eng.ImportRules(cfg.RulesFile); err != nil {
				logger.Warn("rules import failed", "file", cfg.RulesFile, "err", err)
			} else {
				logger.Info("imported rules", "count", n, "file", cfg.RulesFile)
			}
		}
	}

	idx := dockerclient.New(cfg.DockerSocket)
	be, err := detectBackend(ctx, cfg, idx, logger)
	if err != nil {
		logger.Warn("backend detection failed; using native", "err", err)
		be = backend.Native{}
	}
	key, err := authz.LoadOrCreateKey(filepath.Join(cfg.StateDir, "enforce.key"))
	if err != nil {
		reg.Close()
		return nil, err
	}
	alloc := dockerproxy.NewAllocator(reg, cfg.GatewayPortStart, cfg.GatewayPortEnd, be.CanBind, logger)
	proxy := dockerproxy.New(dockerproxy.Config{
		RealSocket:  cfg.DockerSocket,
		ProxySocket: cfg.SocketPath,
		Domain:      cfg.ProjectDomain,
		SignKey:     key,
		SocketGroup: cfg.SocketGroup,
	}, reg, eng, be, idx, alloc, logger)
	caddy := router.New(cfg.CAStorageDir, logger)
	dnsServer := dns.New(cfg.DNSListen, cfg.DNSPort, cfg.ExtraTLDs)
	authzServer := authz.New(cfg.AuthzSocket, key, eng, logger)

	rootCtx, cancel := context.WithCancel(ctx)
	d := &Daemon{
		Cfg: cfg, Log: logger, Reg: reg, Engine: eng, Docker: idx, Backend: be,
		Proxy: proxy, Alloc: alloc, Router: caddy, DNS: dnsServer, Authz: authzServer,
		version: version,
		ctx:     rootCtx, cancel: cancel,
		syncCh: make(chan struct{}, 1), reconcileCh: make(chan struct{}, 1),
		watchedLeases: map[int64]bool{},
	}
	d.API = api.New(reg, eng, cfg, idx, api.Callbacks{
		Reconcile:    d.Reconcile,
		DeleteRoute:  d.DeleteRoute,
		CreateLease:  d.CreateLease,
		DeleteLease:  d.DeleteLease,
		CheckCompose: d.CheckCompose,
		Doctor:       d.Doctor,
		Enforce:      d.Enforce,
		CA:           d.CA,
		Status:       d.StatusExtra,
		Containers:   d.Containers,
	}, logger, version)
	return d, nil
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

func detectBackend(ctx context.Context, cfg *config.Config, idx *dockerclient.Client, log *slog.Logger) (backend.Backend, error) {
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	info, err := idx.Info(c)
	if err != nil {
		return nil, err
	}
	be, err := backend.Detect(cfg.Backend, info, cfg.DockerSocket)
	if err != nil {
		return nil, err
	}
	log.Info("docker backend", "backend", be.Name(), "docker", info.ServerVersion, "os", info.OperatingSystem)
	return be, nil
}

func (d *Daemon) Version() string { return d.version }

func (d *Daemon) Start() error {
	if err := d.Proxy.Start(d.ctx); err != nil {
		return fmt.Errorf("docker proxy: %w", err)
	}
	if err := d.DNS.Start(); err != nil {
		d.Proxy.Close(context.Background())
		return fmt.Errorf("dns: %w", err)
	}
	if err := d.API.StartUnix(d.ctx); err != nil {
		d.DNS.Stop(context.Background())
		d.Proxy.Close(context.Background())
		return err
	}
	dash, err := d.API.StartTCP()
	if err != nil {
		d.API.Close(context.Background())
		d.DNS.Stop(context.Background())
		d.Proxy.Close(context.Background())
		return err
	}
	d.dashDial = strings.TrimPrefix(dash, "http://")

	// First router load: dashboard + any persisted routes. The CA is created
	// here so `gw ca install` works immediately.
	if err := d.syncRouter(d.ctx); err != nil {
		d.Log.Error("initial router config failed", "err", err)
		d.Reg.AddEvent("error", "route", "initial Caddy config failed: "+err.Error(), nil)
	}
	d.Reg.AddEvent("info", "daemon", "proxd started", map[string]any{
		"version": d.version, "backend": d.Backend.Name(), "docker_socket": d.Cfg.DockerSocket,
		"proxy_socket": d.Cfg.SocketPath, "http_port": d.Cfg.HTTPPort, "https_port": d.Cfg.HTTPSPort,
	})

	if v, _ := d.Reg.GetMeta("enforcement"); v == "on" {
		if err := d.Authz.Start(); err != nil {
			d.Log.Error("enforcement enabled but plugin failed to start", "err", err)
			d.Reg.AddEvent("error", "enforcement", err.Error(), nil)
		}
	}

	d.wg.Add(1)
	go d.syncLoop()
	d.wg.Add(1)
	go d.reconcileLoop()
	d.wg.Add(1)
	go d.dockerLoop()
	d.wg.Add(1)
	go d.leaseLoop()
	return nil
}

func (d *Daemon) Close() {
	d.closeOnce.Do(func() {
		d.cancel()
		d.watchMu.Lock()
		d.closing = true
		d.watchMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = d.Authz.Stop(ctx)
		_ = d.API.Close(ctx)
		_ = d.DNS.Stop(ctx)
		_ = d.Proxy.Close(ctx)
		_ = d.Router.Stop()
		d.wg.Wait()
		d.leaseWG.Wait()
		_ = d.Reg.Close()
	})
}

// Wait blocks until ctx is cancelled.
func (d *Daemon) Wait(ctx context.Context) {
	<-ctx.Done()
}

func (d *Daemon) syncLoop() {
	defer d.wg.Done()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-d.syncCh:
			timer := time.NewTimer(50 * time.Millisecond)
			select {
			case <-timer.C:
			case <-d.ctx.Done():
				timer.Stop()
				return
			}
			if err := d.syncRouter(d.ctx); err != nil {
				d.Log.Error("router sync failed", "err", err)
				d.Reg.AddEvent("error", "route", "Caddy rejected config: "+err.Error(), nil)
			}
		}
	}
}

func (d *Daemon) requestSync() {
	select {
	case d.syncCh <- struct{}{}:
	default:
	}
}

func (d *Daemon) requestReconcile() {
	select {
	case d.reconcileCh <- struct{}{}:
	default:
	}
}

func (d *Daemon) dockerLoop() {
	defer d.wg.Done()
	backoff := time.Second
	for {
		select {
		case <-d.ctx.Done():
			return
		default:
		}
		err := d.Proxy.EnsureNetwork(d.ctx)
		if err == nil {
			err = d.Reconcile(d.ctx)
		}
		if err != nil {
			d.setDockerError(err)
			if !isCanceled(err) {
				d.Log.Warn("docker bootstrap failed; retrying", "err", err, "backoff", backoff)
			}
			select {
			case <-d.ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		d.setDockerError(nil)
		backoff = time.Second
		err = d.watchEvents(d.ctx)
		if err != nil && !isCanceled(err) {
			d.Log.Warn("docker event stream ended; reconnecting", "err", err)
			d.Reg.AddEvent("warn", "docker", "event stream lost; reconnecting: "+err.Error(), nil)
		}
		select {
		case <-d.ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}

func isCanceled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (d *Daemon) setDockerError(err error) {
	d.dockerErrMu.Lock()
	defer d.dockerErrMu.Unlock()
	if err == nil {
		d.dockerErr = ""
	} else {
		d.dockerErr = err.Error()
	}
}

func (d *Daemon) getDockerError() string {
	d.dockerErrMu.RLock()
	defer d.dockerErrMu.RUnlock()
	return d.dockerErr
}

func (d *Daemon) watchEvents(ctx context.Context) error {
	resp, err := d.Docker.Events(ctx, time.Now().Unix())
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	d.Reg.AddEvent("info", "docker", "event stream connected", nil)
	err = dockerclient.DecodeEvents(resp.Body, func(ev dockerclient.Event) error {
		d.Log.Debug("docker event", "action", ev.Action, "id", ev.Actor.ID, "name", ev.Actor.Attributes["name"])
		switch ev.Action {
		case "destroy":
			if ev.Actor.ID != "" {
				d.cleanupDestroyed(ev.Actor.ID)
			}
		case "die":
			if ev.Actor.ID != "" {
				d.markDown(ev.Actor.ID)
			}
			d.requestReconcile()
		case "start", "create", "health_status", "health_status: healthy", "health_status: unhealthy":
			d.requestReconcile()
		}
		return nil
	})
	return err
}

// cleanupDestroyed removes routes immediately on a destroy event, before the
// debounced reconcile runs. This keeps Compose recreate loops collision-free.
func (d *Daemon) cleanupDestroyed(containerID string) {
	routes, err := d.Reg.RoutesForContainer(containerID)
	if err != nil || len(routes) == 0 {
		return
	}
	for _, r := range routes {
		if r.Proto == "tcp" && r.GatewayPort != 0 && r.GatewayPort != 443 {
			d.Alloc.Release(d.gatewayKey(r.ContainerName, r.SourcePort))
		}
	}
	_, _ = d.Reg.DeleteRoutesForContainer(containerID)
	_ = d.Reg.DeleteContainerMeta(containerID)
	d.Reg.AddEvent("info", "route", "container destroyed; routes removed", map[string]any{"container_id": containerID})
	d.requestSync()
}

func (d *Daemon) markDown(containerID string) {
	routes, err := d.Reg.RoutesForContainer(containerID)
	if err != nil {
		return
	}
	for _, r := range routes {
		if r.Status != "down" {
			_ = d.Reg.UpdateRouteStatus(r.ID, "down", r.TargetAddr)
		}
	}
	d.requestSync()
}

func (d *Daemon) reconcileLoop() {
	defer d.wg.Done()
	// A single debounced consumer so bursts of Docker events cause one sweep.
	// A slow ticker converges state even if a health or lifecycle event is
	// missed (Docker daemon restarts, dropped streams, healthcheck transitions).
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			if err := d.Reconcile(d.ctx); err != nil && !isCanceled(err) {
				d.Log.Debug("periodic reconcile failed", "err", err)
			}
		case <-d.reconcileCh:
			d.Log.Debug("reconcile requested")
			timer := time.NewTimer(300 * time.Millisecond)
			select {
			case <-timer.C:
			case <-d.ctx.Done():
				timer.Stop()
				return
			}
			if err := d.Reconcile(d.ctx); err != nil {
				d.Log.Error("reconcile failed", "err", err)
			}
		}
	}
}

func (d *Daemon) leaseLoop() {
	defer d.wg.Done()
	// Re-attach watchers after a daemon restart and clean dead leases.
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	d.reapLeases(d.ctx)
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-t.C:
			d.reapLeases(d.ctx)
		}
	}
}
