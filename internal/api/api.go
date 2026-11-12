package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/haasele/prox/internal/composecheck"
	"github.com/haasele/prox/internal/config"
	"github.com/haasele/prox/internal/dockerclient"
	"github.com/haasele/prox/internal/policy"
	"github.com/haasele/prox/internal/registry"
	"github.com/haasele/prox/web"
)

type Status struct {
	Version          string `json:"version"`
	UptimeSeconds    int64  `json:"uptime_seconds"`
	Policy           string `json:"policy"`
	Backend          string `json:"backend"`
	DockerSocket     string `json:"docker_socket"`
	ProxySocket      string `json:"proxy_socket"`
	APISocket        string `json:"api_socket"`
	HTTPPort         int    `json:"http_port"`
	HTTPSPort        int    `json:"https_port"`
	DNSPort          int    `json:"dns_port"`
	DockerOK         bool   `json:"docker_ok"`
	DockerError      string `json:"docker_error,omitempty"`
	Network          string `json:"network,omitempty"`
	Gateway          string `json:"gateway,omitempty"`
	Routes           int    `json:"routes"`
	Rules            int    `json:"rules"`
	Leases           int    `json:"leases"`
	Enforcement      bool   `json:"enforcement"`
	EnforcementError string `json:"enforcement_error,omitempty"`
	CAReady          bool   `json:"ca_ready"`
	DashboardURL     string `json:"dashboard_url"`
	OwnHTTPPorts     []int  `json:"own_http_ports,omitempty"`
	OwnTCPPorts      []int  `json:"own_tcp_ports,omitempty"`
}

type LeaseRequest struct {
	Name    string `json:"name"`
	Host    string `json:"host"`
	Dir     string `json:"dir"`
	Command string `json:"command"`
	Port    int    `json:"port"`
	PID     int    `json:"pid"`
}

type LeaseResponse struct {
	Lease registry.Lease `json:"lease"`
	Route registry.Route `json:"route"`
}

// RouteRequest creates a manual HTTP route to an existing listener, usually a
// container that was started outside prox. Rules still apply on next create;
// this makes the current process reachable now.
type RouteRequest struct {
	Host       string `json:"host"`
	Proto      string `json:"proto"`
	SourcePort int    `json:"source_port"`
	TargetAddr string `json:"target_addr"`
	Name       string `json:"name"`
	Redirect   *bool  `json:"redirect,omitempty"`
}

type EnforcementStatus struct {
	Enabled      bool     `json:"enabled"`
	Running      bool     `json:"running"`
	Socket       string   `json:"socket"`
	Error        string   `json:"error,omitempty"`
	Instructions []string `json:"instructions,omitempty"`
}

type DoctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | warn | fail
	Detail string `json:"detail,omitempty"`
}

type DoctorReport struct {
	Healthy bool          `json:"healthy"`
	Checks  []DoctorCheck `json:"checks"`
}

type ManagedView struct {
	registry.Route
	URL string `json:"url,omitempty"`
}

type UnmanagedView struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Image      string            `json:"image"`
	Project    string            `json:"project,omitempty"`
	Service    string            `json:"service,omitempty"`
	Ports      []string          `json:"ports,omitempty"`
	TargetAddr string            `json:"target_addr,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	Suggestion Suggestion        `json:"suggestion"`
}

type Suggestion struct {
	Name    string `json:"name"`
	Project string `json:"project,omitempty"`
	Source  int    `json:"source,omitempty"`
	Proto   string `json:"proto"`
	Resolve string `json:"resolve,omitempty"`
	Action  string `json:"action"`
}

type ContainersView struct {
	Managed   []ManagedView   `json:"managed"`
	Unmanaged []UnmanagedView `json:"unmanaged"`
	Warnings  []string        `json:"warnings"`
}

type Callbacks struct {
	Reconcile    func(context.Context) error
	CreateRoute  func(context.Context, RouteRequest) (*registry.Route, error)
	DeleteRoute  func(context.Context, int64) error
	CreateLease  func(context.Context, LeaseRequest) (*LeaseResponse, error)
	DeleteLease  func(context.Context, int64) error
	CheckCompose func(context.Context, []byte, string, string) ([]composecheck.Outcome, error)
	Doctor       func(context.Context, bool) (DoctorReport, error)
	Enforce      func(context.Context, bool) (EnforcementStatus, error)
	CA           func(context.Context) ([]byte, error)
	Status       func(context.Context) Status
	Containers   func(context.Context) (ContainersView, error)
}

type API struct {
	Reg     *registry.Registry
	Engine  *policy.Engine
	Cfg     *config.Config
	Docker  *dockerclient.Client
	CB      Callbacks
	Log     *slog.Logger
	Version string
	Started time.Time

	unixLn  net.Listener
	tcpLn   net.Listener
	server  *http.Server
	tcpSrv  *http.Server
	tcpAddr string
}

func New(reg *registry.Registry, eng *policy.Engine, cfg *config.Config, docker *dockerclient.Client, cb Callbacks, log *slog.Logger, version string) *API {
	return &API{Reg: reg, Engine: eng, Cfg: cfg, Docker: docker, CB: cb, Log: log, Version: version, Started: time.Now()}
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/status", a.handleStatus)
	mux.HandleFunc("/v1/rules", a.handleRules)
	mux.HandleFunc("/v1/rules/", a.handleRuleByID)
	mux.HandleFunc("/v1/routes", a.handleRoutes)
	mux.HandleFunc("/v1/routes/", a.handleRouteByID)
	mux.HandleFunc("/v1/leases", a.handleLeases)
	mux.HandleFunc("/v1/leases/", a.handleLeaseByID)
	mux.HandleFunc("/v1/events", a.handleEvents)
	mux.HandleFunc("/v1/events/stream", a.handleEventStream)
	mux.HandleFunc("/v1/policy", a.handlePolicy)
	mux.HandleFunc("/v1/check", a.handleCheck)
	mux.HandleFunc("/v1/containers", a.handleContainers)
	mux.HandleFunc("/v1/suggest", a.handleSuggest)
	mux.HandleFunc("/v1/reconcile", a.handleReconcile)
	mux.HandleFunc("/v1/ca", a.handleCA)
	mux.HandleFunc("/v1/enforce", a.handleEnforce)
	mux.HandleFunc("/v1/doctor", a.handleDoctor)
	mux.HandleFunc("/v1/warnings", a.handleWarnings)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/", a.handleWeb)
	return mux
}

func (a *API) StartUnix(ctx context.Context) error {
	socket := a.Cfg.APISocket
	if err := os.MkdirAll(filepath.Dir(socket), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(socket); err == nil {
		c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}}, Timeout: 300 * time.Millisecond}
		if _, err := c.Get("http://prox/healthz"); err == nil {
			return fmt.Errorf("another prox API is listening on %s", socket)
		}
		_ = os.Remove(socket)
	}
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	if a.Cfg.SocketGroup != "" {
		_ = os.Chmod(socket, 0o660)
		if g, err := user.LookupGroup(a.Cfg.SocketGroup); err == nil {
			if gid, err := strconv.Atoi(g.Gid); err == nil {
				_ = os.Chown(socket, -1, gid)
			}
		}
	} else {
		_ = os.Chmod(socket, 0o600)
	}
	a.unixLn = ln
	a.server = &http.Server{Handler: a.Handler(), ReadHeaderTimeout: 30 * time.Second}
	go func() {
		if err := a.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			a.Log.Error("api unix server", "err", err)
		}
	}()
	return nil
}

// StartTCP serves the dashboard on a loopback ephemeral port; Caddy proxies
// prox.localhost to it.
func (a *API) StartTCP() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	a.tcpLn = ln
	a.tcpAddr = ln.Addr().String()
	a.tcpSrv = &http.Server{Handler: a.Handler(), ReadHeaderTimeout: 30 * time.Second}
	go func() {
		if err := a.tcpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			a.Log.Error("api dashboard server", "err", err)
		}
	}()
	return "http://" + a.tcpAddr, nil
}

func (a *API) TCPAddr() string { return a.tcpAddr }

func (a *API) Close(ctx context.Context) error {
	var first error
	if a.server != nil {
		if err := a.server.Shutdown(ctx); err != nil {
			first = err
		}
	}
	if a.tcpSrv != nil {
		if err := a.tcpSrv.Shutdown(ctx); err != nil && first == nil {
			first = err
		}
	}
	_ = os.Remove(a.Cfg.APISocket)
	return first
}

func (a *API) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	st := a.buildStatus(r.Context())
	writeJSON(w, 200, st)
}

func (a *API) buildStatus(ctx context.Context) Status {
	st := Status{
		Version:       a.Version,
		UptimeSeconds: int64(time.Since(a.Started).Seconds()),
		Policy:        a.Engine.Policy(),
		DockerSocket:  a.Cfg.DockerSocket,
		ProxySocket:   a.Cfg.SocketPath,
		APISocket:     a.Cfg.APISocket,
		HTTPPort:      a.Cfg.HTTPPort,
		HTTPSPort:     a.Cfg.HTTPSPort,
		DNSPort:       a.Cfg.DNSPort,
		DashboardURL:  "https://" + a.Cfg.HTTPSHostPort("prox.localhost"),
		OwnHTTPPorts:  a.Cfg.OwnHTTPPorts,
		OwnTCPPorts:   a.Cfg.OwnTCPPorts,
	}
	if a.CB.Status != nil {
		extra := a.CB.Status(ctx)
		if extra.Backend != "" {
			st.Backend = extra.Backend
		}
		if extra.Network != "" {
			st.Network = extra.Network
		}
		if extra.Gateway != "" {
			st.Gateway = extra.Gateway
		}
		st.Enforcement = extra.Enforcement
		st.EnforcementError = extra.EnforcementError
		st.CAReady = extra.CAReady
		st.DockerOK = extra.DockerOK
		st.DockerError = extra.DockerError
	}
	if a.Docker != nil {
		c, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := a.Docker.Ping(c)
		cancel()
		st.DockerOK = err == nil
		if err != nil {
			st.DockerError = err.Error()
		}
	}
	rules, _ := a.Reg.ListRules()
	routes, _ := a.Reg.ListRoutes()
	leases, _ := a.Reg.ListLeases()
	st.Rules, st.Routes, st.Leases = len(rules), len(routes), len(leases)
	return st
}

type ruleRequest struct {
	Name     string `json:"name"`
	Project  string `json:"project"`
	Source   int    `json:"source"`
	Target   int    `json:"target"`
	Resolve  string `json:"resolve"`
	Proto    string `json:"proto"`
	Action   string `json:"action"`
	Redirect *bool  `json:"redirect,omitempty"`
}

func (a *API) handleRules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		rules, err := a.Reg.ListRules()
		if err != nil {
			writeError(w, 500, err)
			return
		}
		if rules == nil {
			rules = []registry.Rule{}
		}
		writeJSON(w, 200, rules)
	case http.MethodPost:
		var req ruleRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			writeError(w, 400, err)
			return
		}
		action := req.Action
		if action == "" {
			action = "allow"
		}
		rule, err := a.Engine.AddRule(policy.RuleRequest{
			Name: req.Name, Project: req.Project, Source: req.Source, Target: req.Target,
			Resolve: req.Resolve, Proto: req.Proto, Action: action, Redirect: req.Redirect,
		})
		if err != nil {
			writeError(w, 400, err)
			return
		}
		_ = a.Engine.ExportRules(a.Cfg.RulesFile)
		a.Reg.AddEvent("info", "rule", fmt.Sprintf("rule %d: %s %s", rule.ID, rule.Action, rule.Name), map[string]any{
			"id": rule.ID, "name": rule.Name, "project": rule.Project, "action": rule.Action,
			"source": rule.Source, "target": rule.Target, "resolve": rule.Resolve, "proto": rule.Proto,
		})
		writeJSON(w, 201, rule)
	default:
		methodNotAllowed(w)
	}
}

func (a *API) handleRuleByID(w http.ResponseWriter, r *http.Request) {
	id, err := idFromPath(r.URL.Path, "/v1/rules/")
	if err != nil {
		writeError(w, 400, err)
		return
	}
	switch r.Method {
	case http.MethodDelete:
		ok, err := a.Engine.DeleteRule(id)
		if err != nil {
			writeError(w, 500, err)
			return
		}
		if !ok {
			writeError(w, 404, fmt.Errorf("rule %d not found", id))
			return
		}
		_ = a.Engine.ExportRules(a.Cfg.RulesFile)
		a.Reg.AddEvent("info", "rule", fmt.Sprintf("rule %d deleted", id), map[string]any{"id": id})
		writeJSON(w, 200, map[string]any{"deleted": id})
	case http.MethodPut:
		var req ruleRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			writeError(w, 400, err)
			return
		}
		action := req.Action
		if action == "" {
			action = "allow"
		}
		rule, err := a.Engine.UpdateRule(id, policy.RuleRequest{
			Name: req.Name, Project: req.Project, Source: req.Source, Target: req.Target,
			Resolve: req.Resolve, Proto: req.Proto, Action: action, Redirect: req.Redirect,
		})
		if err != nil {
			writeError(w, 400, err)
			return
		}
		_ = a.Engine.ExportRules(a.Cfg.RulesFile)
		a.Reg.AddEvent("info", "rule", fmt.Sprintf("rule %d updated: %s %s", rule.ID, rule.Action, rule.Name), map[string]any{
			"id": rule.ID, "name": rule.Name, "project": rule.Project, "action": rule.Action,
			"source": rule.Source, "target": rule.Target, "resolve": rule.Resolve, "proto": rule.Proto,
		})
		writeJSON(w, 200, rule)
	default:
		methodNotAllowed(w)
	}
}

func (a *API) handleRoutes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		routes, err := a.Reg.ListRoutes()
		if err != nil {
			writeError(w, 500, err)
			return
		}
		if routes == nil {
			routes = []registry.Route{}
		}
		writeJSON(w, 200, routes)
	case http.MethodPost:
		if a.CB.CreateRoute == nil {
			writeError(w, 500, fmt.Errorf("route creation unavailable"))
			return
		}
		var req RouteRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			writeError(w, 400, err)
			return
		}
		route, err := a.CB.CreateRoute(r.Context(), req)
		if err != nil {
			writeError(w, 400, err)
			return
		}
		writeJSON(w, 201, route)
	default:
		methodNotAllowed(w)
	}
}

func (a *API) handleRouteByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		methodNotAllowed(w)
		return
	}
	id, err := idFromPath(r.URL.Path, "/v1/routes/")
	if err != nil {
		writeError(w, 400, err)
		return
	}
	if a.CB.DeleteRoute == nil {
		writeError(w, 500, fmt.Errorf("route deletion unavailable"))
		return
	}
	if err := a.CB.DeleteRoute(r.Context(), id); err != nil {
		writeError(w, 404, err)
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": id})
}

func (a *API) handleLeases(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		leases, err := a.Reg.ListLeases()
		if err != nil {
			writeError(w, 500, err)
			return
		}
		if leases == nil {
			leases = []registry.Lease{}
		}
		writeJSON(w, 200, leases)
	case http.MethodPost:
		var req LeaseRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			writeError(w, 400, err)
			return
		}
		if a.CB.CreateLease == nil {
			writeError(w, 500, fmt.Errorf("lease creation unavailable"))
			return
		}
		resp, err := a.CB.CreateLease(r.Context(), req)
		if err != nil {
			writeError(w, 400, err)
			return
		}
		writeJSON(w, 201, resp)
	default:
		methodNotAllowed(w)
	}
}

func (a *API) handleLeaseByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		methodNotAllowed(w)
		return
	}
	id, err := idFromPath(r.URL.Path, "/v1/leases/")
	if err != nil {
		writeError(w, 400, err)
		return
	}
	if a.CB.DeleteLease == nil {
		writeError(w, 500, fmt.Errorf("lease deletion unavailable"))
		return
	}
	if err := a.CB.DeleteLease(r.Context(), id); err != nil {
		writeError(w, 404, err)
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": id})
}

func (a *API) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := a.Reg.ListEvents(since, limit)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	if events == nil {
		events = []registry.Event{}
	}
	writeJSON(w, 200, events)
}

func (a *API) handleEventStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, fmt.Errorf("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)
	fl.Flush()

	ch, unsub := a.Reg.Bus.Subscribe()
	defer unsub()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			b, _ := json.Marshal(ev)
			_, _ = fmt.Fprintf(w, "id: %d\ndata: %s\n\n", ev.ID, b)
			fl.Flush()
		case <-heartbeat.C:
			_, _ = fmt.Fprintf(w, ": keepalive\n\n")
			fl.Flush()
		}
	}
}

func (a *API) handlePolicy(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, map[string]string{"policy": a.Engine.Policy()})
	case http.MethodPost:
		var body struct {
			Policy string `json:"policy"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
			writeError(w, 400, err)
			return
		}
		if err := a.Engine.SetPolicy(body.Policy); err != nil {
			writeError(w, 400, err)
			return
		}
		a.Reg.AddEvent("info", "policy", "policy set to "+body.Policy, map[string]any{"policy": body.Policy})
		writeJSON(w, 200, map[string]string{"policy": body.Policy})
	default:
		methodNotAllowed(w)
	}
}

func (a *API) handleCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeError(w, 400, err)
		return
	}
	if a.CB.CheckCompose == nil {
		writeError(w, 500, fmt.Errorf("check unavailable"))
		return
	}
	outcomes, err := a.CB.CheckCompose(r.Context(), body, r.URL.Query().Get("filename"), r.URL.Query().Get("project"))
	if err != nil {
		writeError(w, 400, err)
		return
	}
	if outcomes == nil {
		outcomes = []composecheck.Outcome{}
	}
	for i := range outcomes {
		outcomes[i].URL = policy.RouteURL(outcomes[i].Proto, outcomes[i].Host, outcomes[i].GatewayPort, a.Cfg.HTTPSPort)
	}
	writeJSON(w, 200, map[string]any{"project": projectName(outcomes), "services": outcomes})
}

func projectName(outcomes []composecheck.Outcome) string {
	if len(outcomes) > 0 {
		return outcomes[0].Project
	}
	return ""
}

func (a *API) handleContainers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if a.CB.Containers == nil {
		writeJSON(w, 200, ContainersView{})
		return
	}
	view, err := a.CB.Containers(r.Context())
	if err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, 200, view)
}

func (a *API) handleSuggest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	q := r.URL.Query()
	name := q.Get("name")
	if name == "" {
		name = "app"
	}
	port, _ := strconv.Atoi(q.Get("port"))
	image := q.Get("image")
	proto := "http"
	source := port
	if profile, ok := a.Engine.Profiles.Get(name); ok {
		proto = profile.Proto
		if source == 0 {
			source = profile.Port
		}
	} else if profile, ok := a.Engine.ProfileForImage(image); ok && profile.Proto == "tcp" {
		proto = profile.Proto
		if source == 0 {
			source = profile.Port
		}
	}
	s := Suggestion{
		Name: name, Project: q.Get("project"), Source: source, Proto: proto,
		Resolve: policy.NormalizeResolve(name, a.Engine.Domain), Action: "allow",
	}
	writeJSON(w, 200, s)
}

func (a *API) handleWarnings(w http.ResponseWriter, r *http.Request) {
	if a.CB.Containers == nil {
		writeJSON(w, 200, map[string]any{"warnings": []string{}})
		return
	}
	view, err := a.CB.Containers(r.Context())
	if err != nil {
		writeError(w, 500, err)
		return
	}
	warnings := view.Warnings
	if warnings == nil {
		warnings = []string{}
	}
	writeJSON(w, 200, map[string]any{"warnings": warnings})
}

func (a *API) handleReconcile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if a.CB.Reconcile == nil {
		writeError(w, 500, fmt.Errorf("reconcile unavailable"))
		return
	}
	if err := a.CB.Reconcile(r.Context()); err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *API) handleCA(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if a.CB.CA == nil {
		writeError(w, 500, fmt.Errorf("CA unavailable"))
		return
	}
	pem, err := a.CB.CA(r.Context())
	if err != nil {
		writeError(w, 503, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Length", strconv.Itoa(len(pem)))
	_, _ = w.Write(pem)
}

func (a *API) handleEnforce(w http.ResponseWriter, r *http.Request) {
	on := false
	switch r.Method {
	case http.MethodGet:
		if a.CB.Enforce != nil {
			st, _ := a.CB.Enforce(r.Context(), a.enforcementEnabled())
			writeJSON(w, 200, st)
			return
		}
		writeJSON(w, 200, EnforcementStatus{})
		return
	case http.MethodPost:
		var body struct {
			Enabled *bool `json:"enabled"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
			writeError(w, 400, err)
			return
		}
		if body.Enabled == nil {
			writeError(w, 400, fmt.Errorf("enabled is required"))
			return
		}
		on = *body.Enabled
	default:
		methodNotAllowed(w)
		return
	}
	if a.CB.Enforce == nil {
		writeError(w, 500, fmt.Errorf("enforcement unavailable"))
		return
	}
	st, err := a.CB.Enforce(r.Context(), on)
	if err != nil {
		writeJSON(w, 409, st)
		return
	}
	writeJSON(w, 200, st)
}

func (a *API) enforcementEnabled() bool {
	v, _ := a.Reg.GetMeta("enforcement")
	return v == "on"
}

func (a *API) handleDoctor(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	fix := r.URL.Query().Get("fix") == "1"
	if a.CB.Doctor == nil {
		writeError(w, 500, fmt.Errorf("doctor unavailable"))
		return
	}
	rep, err := a.CB.Doctor(r.Context(), fix)
	if err != nil {
		writeError(w, 500, err)
		return
	}
	writeJSON(w, 200, rep)
}

func (a *API) handleWeb(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/v1/") {
		writeError(w, 404, fmt.Errorf("not found"))
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		name = "index.html"
	}
	body, ctype, ok := web.Asset(name)
	if !ok {
		body, ctype, _ = web.Asset("index.html")
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func methodNotAllowed(w http.ResponseWriter) {
	writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("method not allowed"))
}

func idFromPath(path, prefix string) (int64, error) {
	rest := strings.TrimPrefix(path, prefix)
	rest = strings.TrimSuffix(rest, "/")
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid id %q", rest)
	}
	return id, nil
}
