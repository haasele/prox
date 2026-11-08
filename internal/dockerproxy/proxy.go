package dockerproxy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	slog "log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/haasele/prox/internal/backend"
	"github.com/haasele/prox/internal/dockerclient"
	"github.com/haasele/prox/internal/policy"
	"github.com/haasele/prox/internal/registry"
)

const (
	LabelManaged  = "gw.managed"
	LabelProject  = "gw.project"
	LabelService  = "gw.service"
	LabelRule     = "gw.rule"
	LabelHost     = "gw.host"
	LabelProto    = "gw.proto"
	LabelPort     = "gw.port"
	LabelTarget   = "gw.target"
	LabelRedirect = "gw.redirect"
	LabelAction   = "gw.action"

	SignatureHeader = "X-Prox-Signature"
	ManagedHeader   = "X-Prox-Managed"
	NetworkName     = "gw"
	MaxCreateBody   = 32 << 20
	MaxJSONResponse = 64 << 20
)

var versionPrefix = regexp.MustCompile(`^/v[0-9]+\.[0-9]+`)

type Config struct {
	RealSocket  string
	ProxySocket string
	Domain      string
	SignKey     []byte
	SocketGroup string
}

type Proxy struct {
	cfg     Config
	reg     *registry.Registry
	Policy  *policy.Engine
	Backend backend.Backend
	Docker  *dockerclient.Client
	Alloc   *Allocator
	Log     *slog.Logger
	Network string

	gwMu    sync.RWMutex
	Gateway string

	transport  *http.Transport
	rp         *httputil.ReverseProxy
	server     *http.Server
	listener   net.Listener
	imgMu      sync.Mutex
	imgExposed map[string][]int
}

func New(cfg Config, reg *registry.Registry, eng *policy.Engine, be backend.Backend, idx *dockerclient.Client, alloc *Allocator, log *slog.Logger) *Proxy {
	if cfg.Domain == "" {
		cfg.Domain = "localhost"
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", cfg.RealSocket)
		},
		DisableCompression:  true,
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
	}
	p := &Proxy{
		cfg:        cfg,
		reg:        reg,
		Policy:     eng,
		Backend:    be,
		Docker:     idx,
		Alloc:      alloc,
		Log:        log,
		Network:    NetworkName,
		transport:  tr,
		imgExposed: map[string][]int{},
	}
	p.rp = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = "docker"
			pr.Out.Host = "docker"
		},
		Transport:     tr,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			p.Log.Error("docker proxy upstream error", "path", r.URL.Path, "err", err)
			writeAPIError(w, http.StatusBadGateway,
				fmt.Sprintf("cannot reach the Docker daemon at %s: %v. If prox is down, use DOCKER_HOST=unix:///var/run/docker.sock as a fallback.", cfg.RealSocket, err))
		},
	}
	return p
}

func shareSocket(path, group string) {
	g, err := user.LookupGroup(group)
	if err != nil {
		return
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return
	}
	_ = os.Chown(path, -1, gid)
}

func (p *Proxy) Start(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(p.cfg.ProxySocket), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(p.cfg.ProxySocket); err == nil {
		// Refuse to steal a socket from a live daemon.
		c := dockerclient.New(p.cfg.ProxySocket)
		pingCtx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
		err := c.Ping(pingCtx)
		cancel()
		if err == nil {
			return fmt.Errorf("another prox daemon is already listening on %s", p.cfg.ProxySocket)
		}
		_ = os.Remove(p.cfg.ProxySocket)
	}
	ln, err := net.Listen("unix", p.cfg.ProxySocket)
	if err != nil {
		return err
	}
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(true)
	}
	if p.cfg.SocketGroup != "" {
		_ = os.Chmod(p.cfg.ProxySocket, 0o660)
		shareSocket(p.cfg.ProxySocket, p.cfg.SocketGroup)
	} else {
		_ = os.Chmod(p.cfg.ProxySocket, 0o600)
	}
	p.listener = ln
	p.server = &http.Server{
		Handler:           p,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       10 * time.Minute,
		ErrorLog:          slog.NewLogLogger(p.Log.Handler(), slog.LevelWarn),
	}
	go func() {
		if err := p.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			p.Log.Error("docker proxy server", "err", err)
		}
	}()
	return nil
}

func (p *Proxy) Close(ctx context.Context) error {
	if p.server == nil {
		return nil
	}
	err := p.server.Shutdown(ctx)
	_ = os.Remove(p.cfg.ProxySocket)
	return err
}

// EnsureNetwork creates the gw bridge network if it is missing and caches its
// gateway address for container-side name resolution.
// GatewayAddr returns the gw bridge gateway, safe for concurrent readers.
func (p *Proxy) GatewayAddr() string {
	p.gwMu.RLock()
	defer p.gwMu.RUnlock()
	return p.Gateway
}

func (p *Proxy) EnsureNetwork(ctx context.Context) error {
	netw, err := p.Docker.InspectNetwork(ctx, p.Network)
	if err != nil {
		var apiErr *dockerclient.APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
			return err
		}
		if _, err := p.Docker.CreateNetwork(ctx, p.Network, map[string]string{
			LabelManaged: "true",
			"gw.purpose": "gateway",
		}); err != nil {
			return err
		}
		netw, err = p.Docker.InspectNetwork(ctx, p.Network)
		if err != nil {
			return err
		}
	}
	gw := ""
	if len(netw.IPAM.Config) > 0 {
		gw = netw.IPAM.Config[0].Gateway
	}
	p.gwMu.Lock()
	p.Gateway = gw
	p.gwMu.Unlock()
	_ = p.reg.SetMeta("network.name", p.Network)
	_ = p.reg.SetMeta("network.id", netw.ID)
	_ = p.reg.SetMeta("network.gateway", gw)
	return nil
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest := versionPrefix.ReplaceAllString(r.URL.Path, "")
	if rest == "" {
		rest = "/"
	}

	switch {
	case r.Method == http.MethodPost && rest == "/containers/create":
		p.handleCreate(w, r)
		return
	case r.Method == http.MethodGet && rest == "/containers/json":
		p.handleContainerList(w, r)
		return
	case r.Method == http.MethodGet && strings.HasPrefix(rest, "/containers/") && strings.HasSuffix(rest, "/json"):
		p.handleInspect(w, r, rest)
		return
	case r.Method == http.MethodPost && strings.HasPrefix(rest, "/networks/") && strings.HasSuffix(rest, "/connect"):
		p.handleNetworkConnect(w, r)
		return
	}
	p.rp.ServeHTTP(w, r)
}

// ---- Create ----

func (p *Proxy) handleCreate(w http.ResponseWriter, r *http.Request) {
	original, err := io.ReadAll(io.LimitReader(r.Body, MaxCreateBody+1))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "cannot read create body: "+err.Error())
		return
	}
	if len(original) > MaxCreateBody {
		writeAPIError(w, http.StatusRequestEntityTooLarge, "create body too large")
		return
	}
	body, ok := decodeObject(original)
	if !ok {
		// Let Docker produce the authoritative error for malformed bodies.
		p.forwardCreate(w, r, original, original, nil, policy.CreateInfo{})
		return
	}

	info := p.createInfo(r.Context(), r, body)
	decision, err := p.Policy.Decide(info)
	if err != nil {
		p.reg.AddEvent("error", "deny", "create rejected: "+err.Error(), map[string]any{
			"container": info.Name, "project": info.Project, "service": info.Service, "image": info.Image,
		})
		writeAPIError(w, http.StatusForbidden, err.Error())
		return
	}

	switch decision.Action {
	case policy.ActionDeny:
		p.reg.AddEvent("warn", "deny", decision.Reason, map[string]any{
			"container": info.Name, "project": info.Project, "service": info.Service, "image": info.Image,
			"ports": info.ExposedPorts,
		})
		writeAPIError(w, http.StatusForbidden, decision.Reason)
		return
	case policy.ActionWarn:
		p.reg.AddEvent("warn", "warn", decision.Reason, map[string]any{
			"container": info.Name, "project": info.Project, "service": info.Service, "image": info.Image,
		})
		p.forwardCreate(w, r, original, original, nil, policy.CreateInfo{})
		return
	case policy.ActionSkip:
		p.forwardCreate(w, r, original, original, nil, policy.CreateInfo{})
		return
	case policy.ActionPublish:
		rewritten, err := p.applyLabels(body, info, decision)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err.Error())
			return
		}
		p.reg.AddEvent("info", "publish", decision.Reason, map[string]any{
			"container": info.Name, "host": decision.Host, "ports": info.ExposedPorts,
		})
		p.forwardCreate(w, r, original, rewritten, &decision, info)
		return
	}

	mode, _ := stringAt(body, "HostConfig", "NetworkMode")
	if mode == "host" || mode == "none" || strings.HasPrefix(mode, "container:") {
		writeAPIError(w, http.StatusConflict,
			fmt.Sprintf("prox cannot route a container using network mode %q; use a bridge network", mode))
		return
	}

	host, collision, err := p.allocateHost(decision.Host, info.Project, info.Name, decision.Proto)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if collision {
		p.reg.AddEvent("warn", "collision", fmt.Sprintf("host %s is already in use; %s routed as %s", decision.Host, info.Name, host), map[string]any{
			"requested": decision.Host, "assigned": host, "project": info.Project, "container": info.Name,
		})
	}
	decision.Host = host
	decision.Collision = collision

	if decision.Proto == "tcp" && decision.TargetPort != 443 {
		key := p.portKey(info.Name, decision.SourcePort)
		var port int
		var err error
		if decision.ExplicitTarget {
			port, err = p.Alloc.AllocateExplicit(decision.PreferredGatewayPort, key)
		} else {
			port, err = p.Alloc.Allocate(decision.PreferredGatewayPort, key)
		}
		if err != nil {
			writeAPIError(w, http.StatusConflict, "cannot allocate gateway port: "+err.Error())
			return
		}
		decision.TargetPort = port
	}

	rewritten, err := p.rewriteCreate(body, info, decision)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	p.reg.AddEvent("info", "rewrite", fmt.Sprintf("%s -> %s (%s)", containerLabel(info), host, decision.Proto), map[string]any{
		"container": info.Name, "project": info.Project, "service": info.Service, "image": info.Image,
		"host": host, "source_port": decision.SourcePort, "gateway_port": decision.TargetPort,
		"proto": decision.Proto, "rule": decision.RuleID, "collision": collision,
	})
	p.forwardCreate(w, r, original, rewritten, &decision, info)
}

func containerLabel(info policy.CreateInfo) string {
	if info.Service != "" {
		return info.Service
	}
	if info.Name != "" {
		return info.Name
	}
	return info.Image
}

func (p *Proxy) createInfo(ctx context.Context, r *http.Request, body map[string]any) policy.CreateInfo {
	labels := stringMap(getMap(body, "Labels"))
	if len(labels) == 0 {
		labels = stringMap(getMap(body, "Config", "Labels"))
	}
	info := policy.CreateInfo{
		Name:         r.URL.Query().Get("name"),
		Project:      labels["com.docker.compose.project"],
		Service:      labels["com.docker.compose.service"],
		Image:        stringAtDefault(body, "", "Image"),
		Labels:       labels,
		Bindings:     parseBindings(body),
		ExposedPorts: parseExposedPorts(body),
	}
	info.PublishAll, _ = boolAt(body, "HostConfig", "PublishAllPorts")
	info.NetworkMode, _ = stringAt(body, "HostConfig", "NetworkMode")
	if info.Name == "" {
		info.Name = labels["com.docker.compose.container-number"]
	}
	if info.Image != "" {
		info.ImageExposedPorts = p.imageExposedPorts(ctx, info.Image)
	}
	return info
}

// imageExposedPorts reads the image's EXPOSE list. The spec prefers it over
// create-body bindings for port detection, so `-p 3000:3000 nginx` still
// reaches nginx's real port 80.
func (p *Proxy) imageExposedPorts(ctx context.Context, image string) []int {
	p.imgMu.Lock()
	if v, ok := p.imgExposed[image]; ok {
		p.imgMu.Unlock()
		return v
	}
	p.imgMu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	info, err := p.Docker.InspectImage(cctx, image)
	if err != nil {
		p.Log.Debug("image inspect failed", "image", image, "err", err)
		return nil
	}
	var ports []int
	for key := range info.Config.ExposedPorts {
		port, proto := splitPort(key)
		if proto == "tcp" && port > 0 {
			ports = append(ports, port)
		}
	}
	p.imgMu.Lock()
	p.imgExposed[image] = ports
	p.imgMu.Unlock()
	return ports
}

func (p *Proxy) applyLabels(body map[string]any, info policy.CreateInfo, d policy.Decision) ([]byte, error) {
	labels := stringMapAny(body["Labels"])
	if len(labels) == 0 {
		labels = stringMapAny(getMap(body, "Config")["Labels"])
	}
	labels[LabelManaged] = "true"
	labels[LabelProject] = info.Project
	labels[LabelService] = info.Service
	labels[LabelRule] = strconv.FormatInt(d.RuleID, 10)
	labels[LabelAction] = d.Action
	if d.Host != "" {
		labels[LabelHost] = d.Host
		labels[LabelProto] = d.Proto
		labels[LabelPort] = strconv.Itoa(d.SourcePort)
		labels[LabelTarget] = strconv.Itoa(d.TargetPort)
		labels[LabelRedirect] = strconv.FormatBool(d.Redirect)
	}
	body["Labels"] = labels
	return json.Marshal(body)
}

func (p *Proxy) rewriteCreate(body map[string]any, info policy.CreateInfo, d policy.Decision) ([]byte, error) {
	hostConfig := getMap(body, "HostConfig")
	if hostConfig == nil {
		hostConfig = map[string]any{}
		body["HostConfig"] = hostConfig
	}
	if p.Backend != nil && p.Backend.RequiresPublishedPorts() {
		backend.AddLoopbackBindings(hostConfig, []int{d.SourcePort})
	} else {
		delete(hostConfig, "PortBindings")
	}
	hostConfig["PublishAllPorts"] = false

	// Attach gw while keeping networks Compose already asked for.
	endpoints := getMap(body, "NetworkingConfig", "EndpointsConfig")
	if endpoints == nil {
		ncfg, _ := body["NetworkingConfig"].(map[string]any)
		if ncfg == nil {
			ncfg = map[string]any{}
			body["NetworkingConfig"] = ncfg
		}
		endpoints = map[string]any{}
		ncfg["EndpointsConfig"] = endpoints
	}
	if _, ok := endpoints[p.Network]; !ok {
		endpoints[p.Network] = map[string]any{}
	}

	if err := p.addExtraHosts(hostConfig, d); err != nil {
		p.Log.Warn("extra hosts rewrite", "err", err)
	}

	rewritten, err := p.applyLabels(body, info, d)
	if err != nil {
		return nil, err
	}
	return rewritten, nil
}

func (p *Proxy) addExtraHosts(hostConfig map[string]any, d policy.Decision) error {
	hosts := anySlice(hostConfig["ExtraHosts"])
	seen := map[string]bool{}
	for _, h := range hosts {
		if s, ok := h.(string); ok {
			seen[strings.SplitN(s, ":", 2)[0]] = true
		}
	}
	add := func(name, ip string) {
		if name == "" || ip == "" || seen[name] {
			return
		}
		seen[name] = true
		hosts = append(hosts, name+":"+ip)
	}
	add("gw.localhost", "host-gateway")
	gw := p.GatewayAddr()
	if gw != "" {
		add("host.docker.internal", gw)
		routes, err := p.reg.ListRoutes()
		if err == nil {
			for _, rt := range routes {
				if rt.Proto == "http" || rt.Proto == "sni" {
					add(rt.Host, gw)
				}
			}
		}
	}
	if len(hosts) > 0 {
		hostConfig["ExtraHosts"] = hosts
	}
	return nil
}

func (p *Proxy) portKey(name string, source int) string {
	return "name:" + name + ":" + strconv.Itoa(source)
}

func (p *Proxy) allocateHost(proposed, project, containerName, proto string) (string, bool, error) {
	if proposed == "" {
		proposed = policy.DefaultHost(containerName, project, p.cfg.Domain)
	}
	reserved := false
	if (proto == "http" || proto == "sni") && proposed == policy.DashboardHost {
		proposed = policy.UniqueHost(proposed, func(h string) bool { return h == policy.DashboardHost })
		reserved = true
	}
	route, err := p.reg.GetRouteByHost(proposed, proto)
	if err == nil {
		if route.ContainerName == containerName || route.ContainerID == containerName {
			return proposed, route.Collision, nil
		}
		candidate := policy.CollisionHost(proposed, project, p.cfg.Domain)
		taken := func(h string) bool {
			_, err := p.reg.GetRouteByHost(h, proto)
			return err == nil
		}
		host := policy.UniqueHost(candidate, taken)
		return host, true, nil
	}
	return proposed, reserved, nil
}

func (p *Proxy) forwardCreate(w http.ResponseWriter, r *http.Request, original, body []byte, d *policy.Decision, info policy.CreateInfo) {
	headers := r.Header.Clone()
	headers.Set("Content-Type", "application/json")
	headers.Set("Content-Length", strconv.Itoa(len(body)))
	if p.cfg.SignKey != nil && d != nil && (d.Action == policy.ActionRouteHTTP || d.Action == policy.ActionRouteTCP || d.Action == policy.ActionPublish) {
		headers.Set(ManagedHeader, "1")
		headers.Set(SignatureHeader, sign(p.cfg.SignKey, body))
	}
	outReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "http://docker"+r.URL.RequestURI(), bytes.NewReader(body))
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	outReq.Header = headers
	outReq.Host = "docker"
	outReq.ContentLength = int64(len(body))
	resp, err := p.transport.RoundTrip(outReq)
	if err != nil {
		p.Log.Error("create upstream", "err", err)
		writeAPIError(w, http.StatusBadGateway, "cannot reach Docker: "+err.Error())
		return
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, "read create response: "+err.Error())
		return
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && len(respBody) > 0 && d != nil {
		var cr struct {
			ID string `json:"Id"`
		}
		if json.Unmarshal(respBody, &cr) == nil && cr.ID != "" {
			p.persistCreate(r, cr.ID, original, body, d, info)
		}
	} else if d != nil && d.Proto == "tcp" && d.TargetPort != 443 && resp.StatusCode >= 300 {
		p.Alloc.Release(p.portKey(info.Name, d.SourcePort))
	}
	copyResponse(w, resp, respBody)
}

func (p *Proxy) persistCreate(r *http.Request, id string, original, rewritten []byte, d *policy.Decision, info policy.CreateInfo) {
	decisionJSON, _ := json.Marshal(d)
	meta := &registry.ContainerMeta{
		ID:        id,
		Name:      info.Name,
		Project:   info.Project,
		Service:   info.Service,
		Image:     info.Image,
		Original:  original,
		Rewritten: rewritten,
		Decision:  decisionJSON,
	}
	if err := p.reg.SaveContainerMeta(meta); err != nil {
		p.Log.Error("save container meta", "err", err)
		return
	}
	if d.Action != policy.ActionRouteHTTP && d.Action != policy.ActionRouteTCP {
		return
	}
	route := &registry.Route{
		Host:          d.Host,
		ContainerID:   id,
		ContainerName: info.Name,
		Project:       info.Project,
		Service:       info.Service,
		Image:         info.Image,
		Proto:         d.Proto,
		SourcePort:    d.SourcePort,
		GatewayPort:   d.TargetPort,
		RuleID:        d.RuleID,
		Status:        "pending",
		Collision:     d.Collision,
		Redirect:      d.Redirect,
	}
	if err := p.reg.CreateRoute(route); err != nil {
		// Compose recreates containers with the same name; if the old
		// container's destroy event has not been processed yet, take over its
		// route instead of renaming the hostname.
		if existing, gerr := p.reg.GetRouteByHost(d.Host, d.Proto); gerr == nil && existing.ContainerName == info.Name {
			if rerr := p.reg.ReassignRoute(existing.ID, route); rerr == nil {
				return
			}
		}
		p.Log.Error("create pending route", "err", err, "host", d.Host)
		// A racing create may have taken the host. Re-allocate once.
		candidate := policy.CollisionHost(d.Host, info.Project, p.cfg.Domain)
		taken := func(h string) bool { _, e := p.reg.GetRouteByHost(h, d.Proto); return e == nil }
		route.Host = policy.UniqueHost(candidate, taken)
		route.Collision = true
		if err := p.reg.CreateRoute(route); err != nil {
			p.Log.Error("create pending route retry", "err", err)
			return
		}
		p.reg.AddEvent("warn", "collision", fmt.Sprintf("host %s was taken; %s routed as %s", d.Host, info.Name, route.Host), map[string]any{
			"requested": d.Host, "assigned": route.Host, "project": info.Project,
		})
	}
}

// ---- Inspect / list rewriting ----

func (p *Proxy) handleInspect(w http.ResponseWriter, r *http.Request, rest string) {
	resp, body, err := p.forwardBuffered(w, r, nil)
	if err != nil || resp == nil {
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(rest, "/containers/"), "/json")
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err == nil {
		if v, ok := obj["Id"].(string); ok && v != "" {
			id = v
		}
		if meta, err := p.reg.GetContainerMeta(id); err == nil {
			rewriteInspect(obj, meta)
			if b, err := json.Marshal(obj); err == nil {
				body = b
			}
		}
	}
	copyResponse(w, resp, body)
}

func (p *Proxy) handleContainerList(w http.ResponseWriter, r *http.Request) {
	resp, body, err := p.forwardBuffered(w, r, nil)
	if err != nil || resp == nil {
		return
	}
	var list []map[string]any
	if err := json.Unmarshal(body, &list); err == nil {
		changed := false
		for _, obj := range list {
			id, _ := obj["Id"].(string)
			if id == "" {
				continue
			}
			meta, err := p.reg.GetContainerMeta(id)
			if err != nil {
				continue
			}
			rewriteSummary(obj, meta)
			changed = true
		}
		if changed {
			if b, err := json.Marshal(list); err == nil {
				body = b
			}
		}
	}
	copyResponse(w, resp, body)
}

func (p *Proxy) forwardBuffered(w http.ResponseWriter, r *http.Request, body []byte) (*http.Response, []byte, error) {
	headers := r.Header.Clone()
	headers.Set("Content-Type", "application/json")
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, "http://docker"+r.URL.RequestURI(), rdr)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return nil, nil, err
	}
	outReq.Header = headers
	outReq.Host = "docker"
	resp, err := p.transport.RoundTrip(outReq)
	if err != nil {
		p.rp.ErrorHandler(w, r, err)
		return nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxJSONResponse))
	if err != nil {
		writeAPIError(w, http.StatusBadGateway, err.Error())
		return nil, nil, err
	}
	return resp, b, nil
}

func (p *Proxy) handleNetworkConnect(w http.ResponseWriter, r *http.Request) {
	resp, body, err := p.forwardBuffered(w, r, nil)
	if err != nil || resp == nil {
		return
	}
	// Docker answers 403 when the container is already attached to gw, which
	// can happen on Compose retries; make it idempotent for our network.
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusConflict {
		var ej struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &ej)
		if strings.Contains(strings.ToLower(ej.Message), "already exists") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	copyResponse(w, resp, body)
}

func rewriteInspect(obj map[string]any, meta *registry.ContainerMeta) {
	orig, ok := decodeObject(meta.Original)
	if !ok {
		return
	}
	hostConfig := getMap(obj, "HostConfig")
	if hostConfig == nil {
		hostConfig = map[string]any{}
		obj["HostConfig"] = hostConfig
	}
	origHS := getMap(orig, "HostConfig")
	if pb, ok := origHS["PortBindings"]; ok {
		hostConfig["PortBindings"] = pb
	} else {
		delete(hostConfig, "PortBindings")
	}
	if pa, ok := origHS["PublishAllPorts"]; ok {
		hostConfig["PublishAllPorts"] = pa
	}
	if nm, ok := origHS["NetworkMode"]; ok {
		hostConfig["NetworkMode"] = nm
	}
	exposed := orig["ExposedPorts"]
	if exposed == nil {
		exposed = getMap(orig, "Config")["ExposedPorts"]
	}
	if exposed != nil {
		cfg := getMap(obj, "Config")
		if cfg == nil {
			cfg = map[string]any{}
			obj["Config"] = cfg
		}
		cfg["ExposedPorts"] = exposed
	}
	ns := getMap(obj, "NetworkSettings")
	if ns == nil {
		ns = map[string]any{}
		obj["NetworkSettings"] = ns
	}
	ns["Ports"] = derivedPorts(orig)
}

func rewriteSummary(obj map[string]any, meta *registry.ContainerMeta) {
	orig, ok := decodeObject(meta.Original)
	if !ok {
		return
	}
	obj["Ports"] = derivedPortList(orig)
}

func derivedPorts(orig map[string]any) map[string]any {
	out := map[string]any{}
	hs := getMap(orig, "HostConfig")
	bindings := getMap(hs, "PortBindings")
	for port, val := range bindings {
		out[port] = val
	}
	exposed, _ := orig["ExposedPorts"].(map[string]any)
	if exposed == nil {
		exposed, _ = getMap(orig, "Config")["ExposedPorts"].(map[string]any)
	}
	for port := range exposed {
		if _, exists := out[port]; !exists {
			out[port] = nil
		}
	}
	return out
}

func derivedPortList(orig map[string]any) []any {
	hs := getMap(orig, "HostConfig")
	bindings := getMap(hs, "PortBindings")
	var out []any
	for port, val := range bindings {
		priv, proto := splitPort(port)
		entries, _ := val.([]any)
		if len(entries) == 0 {
			out = append(out, map[string]any{"PrivatePort": priv, "Type": proto})
			continue
		}
		for _, e := range entries {
			em, _ := e.(map[string]any)
			hp := intValue(em["HostPort"])
			out = append(out, map[string]any{
				"IP": stringValue(em["HostIp"]), "PrivatePort": priv, "PublicPort": hp, "Type": proto,
			})
		}
	}
	exposed, _ := orig["ExposedPorts"].(map[string]any)
	if exposed == nil {
		exposed, _ = getMap(orig, "Config")["ExposedPorts"].(map[string]any)
	}
	{
		for key := range exposed {
			found := false
			for _, o := range out {
				m, _ := o.(map[string]any)
				if intValue(m["PrivatePort"]) == mustPort(key) {
					found = true
					break
				}
			}
			if !found {
				priv, proto := splitPort(key)
				out = append(out, map[string]any{"PrivatePort": priv, "Type": proto})
			}
		}
	}
	return out
}

// ---- helpers ----

func decodeObject(b []byte) (map[string]any, bool) {
	var obj map[string]any
	if err := json.Unmarshal(b, &obj); err != nil {
		return nil, false
	}
	return obj, true
}

func getMap(obj map[string]any, keys ...string) map[string]any {
	cur := obj
	for i, k := range keys {
		v, ok := cur[k]
		if !ok {
			return nil
		}
		next, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		cur = next
		_ = i
	}
	return cur
}

func stringAt(obj map[string]any, keys ...string) (string, bool) {
	if len(keys) == 0 {
		return "", false
	}
	parent := obj
	if len(keys) > 1 {
		parent = getMap(obj, keys[:len(keys)-1]...)
	}
	if parent == nil {
		return "", false
	}
	v, ok := parent[keys[len(keys)-1]].(string)
	return v, ok
}

func stringAtDefault(obj map[string]any, def string, keys ...string) string {
	if v, ok := stringAt(obj, keys...); ok {
		return v
	}
	return def
}

func boolAt(obj map[string]any, keys ...string) (bool, bool) {
	parent := obj
	if len(keys) > 1 {
		parent = getMap(obj, keys[:len(keys)-1]...)
	}
	if parent == nil {
		return false, false
	}
	v, ok := parent[keys[len(keys)-1]].(bool)
	return v, ok
}

func stringMap(m map[string]any) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

func stringMapAny(v any) map[string]string {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]string{}
	}
	return stringMap(m)
}

func anySlice(v any) []any {
	if s, ok := v.([]any); ok {
		return s
	}
	return nil
}

func parseBindings(body map[string]any) map[int][]policy.PortBinding {
	out := map[int][]policy.PortBinding{}
	hs := getMap(body, "HostConfig")
	pb := getMap(hs, "PortBindings")
	for key, val := range pb {
		port, proto := splitPort(key)
		if proto != "tcp" || port == 0 {
			continue
		}
		entries, _ := val.([]any)
		for _, e := range entries {
			em, _ := e.(map[string]any)
			out[port] = append(out[port], policy.PortBinding{
				HostIP:   stringValue(em["HostIp"]),
				HostPort: stringValue(em["HostPort"]),
			})
		}
	}
	return out
}

func parseExposedPorts(body map[string]any) []int {
	var out []int
	exposed, _ := body["ExposedPorts"].(map[string]any)
	if exposed == nil {
		exposed, _ = getMap(body, "Config")["ExposedPorts"].(map[string]any)
	}
	for key := range exposed {
		port, proto := splitPort(key)
		if proto == "tcp" && port > 0 {
			out = append(out, port)
		}
	}
	return out
}

func splitPort(s string) (int, string) {
	proto := "tcp"
	if i := strings.Index(s, "/"); i >= 0 {
		proto = s[i+1:]
		s = s[:i]
	}
	n, _ := strconv.Atoi(s)
	return n, proto
}

func mustPort(s string) int {
	n, _ := splitPort(s)
	return n
}

func stringValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func intValue(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		i, _ := strconv.Atoi(n)
		return i
	}
	return 0
}

func sign(key, body []byte) string {
	m := hmac.New(sha256.New, key)
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

func VerifySignature(key, body []byte, sig string) bool {
	want := sign(key, body)
	return hmac.Equal([]byte(want), []byte(sig))
}

func copyResponse(w http.ResponseWriter, resp *http.Response, body []byte) {
	for k, vs := range resp.Header {
		if strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Transfer-Encoding") || strings.EqualFold(k, "Connection") {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

func writeAPIError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"message": "prox: " + message})
}
