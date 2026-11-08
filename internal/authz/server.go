package authz

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/haasele/prox/internal/policy"
)

type Request struct {
	User            string            `json:"User,omitempty"`
	UserAuthNMethod string            `json:"UserAuthNMethod,omitempty"`
	RequestMethod   string            `json:"RequestMethod,omitempty"`
	RequestURI      string            `json:"RequestUri,omitempty"`
	RequestBody     []byte            `json:"RequestBody,omitempty"`
	RequestHeaders  map[string]string `json:"RequestHeaders,omitempty"`
	ResponseBody    []byte            `json:"ResponseBody,omitempty"`
	ResponseHeaders map[string]string `json:"ResponseHeaders,omitempty"`
	ResponseStatus  int               `json:"ResponseStatusCode,omitempty"`
}

type Response struct {
	Allow bool   `json:"Allow"`
	Msg   string `json:"Msg,omitempty"`
	Err   string `json:"Err,omitempty"`
}

// Server implements the legacy Docker authorization plugin API on a unix
// socket. Docker discovers plugins in /run/docker/plugins.
type Server struct {
	Socket string
	Key    []byte
	Policy *policy.Engine
	Log    *slog.Logger

	mu      sync.Mutex
	ln      net.Listener
	srv     *http.Server
	active  bool
	lastErr string
}

func New(socket string, key []byte, eng *policy.Engine, log *slog.Logger) *Server {
	return &Server{Socket: socket, Key: key, Policy: eng, Log: log}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	activate := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Implements":["authz"]}`))
	}
	mux.HandleFunc("/Plugin.Activate", activate)
	mux.HandleFunc("/AuthZPlugin.AuthZReq", s.authzReq)
	mux.HandleFunc("/Plugin.AuthZPlugin.AuthZReq", s.authzReq)
	mux.HandleFunc("/AuthZPlugin.AuthZRes", s.authzRes)
	mux.HandleFunc("/Plugin.AuthZPlugin.AuthZRes", s.authzRes)
	return mux
}

func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.Socket), 0o755); err != nil {
		s.lastErr = fmt.Sprintf("cannot create Docker plugin directory %s: %v. Run: sudo mkdir -p /run/docker/plugins && sudo chown $USER /run/docker/plugins", filepath.Dir(s.Socket), err)
		return fmt.Errorf("%s", s.lastErr)
	}
	if err := os.Remove(s.Socket); err != nil && !os.IsNotExist(err) {
		s.lastErr = fmt.Sprintf("remove stale plugin socket: %v", err)
		return fmt.Errorf("%s", s.lastErr)
	}
	ln, err := net.Listen("unix", s.Socket)
	if err != nil {
		s.lastErr = fmt.Sprintf("listen on %s: %v (needs write access to /run/docker/plugins)", s.Socket, err)
		return fmt.Errorf("%s", s.lastErr)
	}
	_ = os.Chmod(s.Socket, 0o660)
	s.ln = ln
	s.srv = &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	s.active = true
	s.lastErr = ""
	go func() {
		if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.Log.Error("authz plugin server", "err", err)
		}
	}()
	return nil
}

func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return nil
	}
	err := s.srv.Shutdown(ctx)
	_ = os.Remove(s.Socket)
	s.active = false
	s.ln = nil
	s.srv = nil
	return err
}

type Status struct {
	Enabled bool   `json:"enabled"`
	Socket  string `json:"socket"`
	Error   string `json:"error,omitempty"`
	Running bool   `json:"running"`
}

func (s *Server) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Status{Enabled: s.active, Socket: s.Socket, Error: s.lastErr, Running: s.active}
}

func (s *Server) authzReq(w http.ResponseWriter, r *http.Request) {
	var req Request
	if err := json.NewDecoder(io.LimitReader(r.Body, 32<<20)).Decode(&req); err != nil {
		writeResp(w, Response{Allow: false, Err: "prox authz: malformed request: " + err.Error()})
		return
	}
	writeResp(w, s.evaluate(req))
}

func (s *Server) authzRes(w http.ResponseWriter, r *http.Request) {
	writeResp(w, Response{Allow: true})
}

func (s *Server) evaluate(req Request) Response {
	path := req.RequestURI
	if i := strings.Index(path, "?"); i >= 0 {
		path = path[:i]
	}
	// Strip any /v1.xx prefix for matching.
	isCreate := false
	if i := strings.Index(path, "/containers/create"); i >= 0 {
		path = path[i:]
		isCreate = path == "/containers/create"
	}
	if req.RequestMethod != http.MethodPost || !isCreate {
		return Response{Allow: true}
	}
	var body map[string]any
	if err := json.Unmarshal(req.RequestBody, &body); err != nil {
		return Response{Allow: true}
	}
	hostConfig := mapAt(body, "HostConfig")
	bindings := mapAt(hostConfig, "PortBindings")
	publishAll, _ := hostConfig["PublishAllPorts"].(bool)
	mode, _ := hostConfig["NetworkMode"].(string)
	hostNet := mode == "host" || strings.HasPrefix(mode, "container:")
	if len(bindings) == 0 && !publishAll && !hostNet {
		return Response{Allow: true}
	}
	if sig := headerValue(req.RequestHeaders, "X-Prox-Signature"); sig != "" && s.Key != nil {
		mac := hmac.New(sha256.New, s.Key)
		mac.Write(req.RequestBody)
		want := hex.EncodeToString(mac.Sum(nil))
		if hmac.Equal([]byte(want), []byte(sig)) {
			return Response{Allow: true}
		}
	}
	info := createInfo(req.RequestURI, body)
	if s.Policy != nil && s.Policy.AllowsDirectPublish(info) {
		return Response{Allow: true, Msg: "prox publish rule"}
	}
	name := info.Service
	if name == "" {
		name = strings.TrimPrefix(info.Name, "/")
	}
	ports := portKeys(bindings)
	hint := "gw allow " + name + " --publish"
	if len(ports) > 0 {
		hint = fmt.Sprintf("gw allow %s --source %s --publish", name, strings.Split(ports[0], "/")[0])
	}
	what := "direct host port binding"
	if len(ports) == 0 && hostNet {
		what = "host/container network mode"
	}
	msg := fmt.Sprintf("prox enforcement: %s is not allowed for %q (%s). Point DOCKER_HOST at the prox proxy socket, or add a publish rule: %s", what, name, strings.Join(ports, ", "), hint)
	s.Log.Warn("authz denied", "container", name, "ports", ports)
	return Response{Allow: false, Msg: msg}
}

func createInfo(uri string, body map[string]any) policy.CreateInfo {
	labels := map[string]string{}
	labelMap, _ := body["Labels"].(map[string]any)
	if len(labelMap) == 0 {
		labelMap, _ = mapAt(body, "Config")["Labels"].(map[string]any)
	}
	for k, v := range labelMap {
		if s, ok := v.(string); ok {
			labels[k] = s
		}
	}
	name := ""
	if i := strings.Index(uri, "name="); i >= 0 {
		name = uri[i+5:]
		if j := strings.Index(name, "&"); j >= 0 {
			name = name[:j]
		}
	}
	info := policy.CreateInfo{
		Name:     name,
		Project:  labels["com.docker.compose.project"],
		Service:  labels["com.docker.compose.service"],
		Labels:   labels,
		Bindings: map[int][]policy.PortBinding{},
	}
	if img, ok := body["Image"].(string); ok {
		info.Image = img
	}
	hc := mapAt(body, "HostConfig")
	if b, ok := hc["PortBindings"].(map[string]any); ok {
		for key, val := range b {
			port := 0
			if i := strings.Index(key, "/"); i > 0 {
				fmt.Sscanf(key[:i], "%d", &port)
			} else {
				fmt.Sscanf(key, "%d", &port)
			}
			entries, _ := val.([]any)
			for _, e := range entries {
				em, _ := e.(map[string]any)
				pb := policy.PortBinding{}
				if ip, ok := em["HostIp"].(string); ok {
					pb.HostIP = ip
				}
				if hp, ok := em["HostPort"].(string); ok {
					pb.HostPort = hp
				}
				info.Bindings[port] = append(info.Bindings[port], pb)
			}
		}
	}
	exposed, _ := body["ExposedPorts"].(map[string]any)
	if exposed == nil {
		exposed, _ = mapAt(body, "Config")["ExposedPorts"].(map[string]any)
	}
	if len(exposed) > 0 {
		for key := range exposed {
			port := 0
			fmt.Sscanf(strings.SplitN(key, "/", 2)[0], "%d", &port)
			if port > 0 {
				info.ExposedPorts = append(info.ExposedPorts, port)
			}
		}
	}
	if mode, ok := hc["NetworkMode"].(string); ok {
		info.NetworkMode = mode
	}
	return info
}

func mapAt(m map[string]any, key string) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	v, _ := m[key].(map[string]any)
	if v == nil {
		return map[string]any{}
	}
	return v
}

func headerValue(headers map[string]string, key string) string {
	for k, v := range headers {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}

func portKeys(bindings map[string]any) []string {
	out := make([]string, 0, len(bindings))
	for k := range bindings {
		out = append(out, k)
	}
	return out
}

func writeResp(w http.ResponseWriter, resp Response) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// LoadOrCreateKey returns a stable HMAC key for signing proxied create bodies.
func LoadOrCreateKey(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil && len(b) >= 16 {
		return b, nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}
