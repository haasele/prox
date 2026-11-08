//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var (
	buildOnce sync.Once
	buildBin  string
	buildErr  error
)

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func binary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "prox-e2e-bin-*")
		if err != nil {
			buildErr = err
			return
		}
		buildBin = filepath.Join(dir, "prox")
		cmd := exec.Command("go", "build", "-o", buildBin, "./cmd/prox")
		cmd.Dir = repoRoot()
		out, err := cmd.CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("build prox: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return buildBin
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func freeUDPPort(t *testing.T) int {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	return pc.LocalAddr().(*net.UDPAddr).Port
}

func randSuffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func dockerSocket() string {
	if h := os.Getenv("DOCKER_HOST"); strings.HasPrefix(h, "unix://") && !strings.Contains(h, "prox-e2e") {
		return strings.TrimPrefix(h, "unix://")
	}
	return "/var/run/docker.sock"
}

func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(dockerSocket()); err != nil {
		t.Skipf("Docker socket unavailable at %s: %v", dockerSocket(), err)
	}
	cmd := exec.Command("docker", "version", "--format", "{{.Server.Version}}")
	cmd.Env = append(os.Environ(), "DOCKER_HOST=unix://"+dockerSocket())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("Docker unavailable: %v %s", err, out)
	}
}

type Harness struct {
	T            *testing.T
	Bin          string
	StateDir     string
	RuntimeDir   string
	ProxySocket  string
	APISocket    string
	AuthzSocket  string
	HTTPPort     int
	HTTPSPort    int
	LoopPort     int
	DNSPort      int
	DashboardURL string
	Env          []string

	cmd    *exec.Cmd
	logf   *os.File
	caPEM  []byte
	caOnce sync.Once
	client *http.Client
}

func NewHarness(t *testing.T) *Harness {
	t.Helper()
	requireDocker(t)
	base := t.TempDir()
	h := &Harness{
		T: t, Bin: binary(t),
		StateDir:   filepath.Join(base, "state"),
		RuntimeDir: filepath.Join(base, "run"),
		HTTPPort:   freePort(t),
		HTTPSPort:  freePort(t),
		DNSPort:    freeUDPPort(t),
	}
	h.LoopPort = freePort(t)
	for h.LoopPort == h.HTTPSPort || h.LoopPort == h.HTTPPort {
		h.LoopPort = freePort(t)
	}
	h.ProxySocket = filepath.Join(h.RuntimeDir, "docker.sock")
	h.APISocket = filepath.Join(h.RuntimeDir, "api.sock")
	h.AuthzSocket = filepath.Join(base, "authz.sock")
	h.DashboardURL = fmt.Sprintf("https://gw.localhost:%d/", h.HTTPSPort)
	h.Env = append(os.Environ(),
		"PROX_STATE_DIR="+h.StateDir,
		"PROX_RUNTIME_DIR="+h.RuntimeDir,
		"PROX_HTTP_PORT="+fmt.Sprint(h.HTTPPort),
		"PROX_HTTPS_PORT="+fmt.Sprint(h.HTTPSPort),
		"PROX_TLS_LOOPBACK_PORT="+fmt.Sprint(h.LoopPort),
		"PROX_DNS_PORT="+fmt.Sprint(h.DNSPort),
		"PROX_DOCKER_SOCKET="+h.ProxySocket,
		"PROX_API_SOCKET="+h.APISocket,
		"PROX_AUTHZ_SOCKET="+h.AuthzSocket,
		"PROX_RULES_FILE="+filepath.Join(base, "rules.yaml"),
		"PROX_PROFILES_DIR="+filepath.Join(base, "profiles.d"),
		"PROX_CA_DIR="+filepath.Join(base, "ca"),
		"PROX_REAL_DOCKER_SOCKET="+dockerSocket(),
		"PROX_LOG_LEVEL=debug",
		"PROX_ENFORCE=false",
	)
	h.client = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", h.APISocket)
		},
		DisableCompression: true,
	}, Timeout: 20 * time.Second}
	h.StartDaemon()
	t.Cleanup(func() { h.StopDaemon() })
	return h
}

func (h *Harness) StartDaemon() {
	h.T.Helper()
	logPath := filepath.Join(h.StateDir, fmt.Sprintf("proxd-%d.log", time.Now().UnixNano()))
	_ = os.MkdirAll(h.StateDir, 0o755)
	f, err := os.Create(logPath)
	if err != nil {
		h.T.Fatal(err)
	}
	h.logf = f
	cmd := exec.Command(h.Bin, "daemon")
	cmd.Env = h.Env
	cmd.Stdout = f
	cmd.Stderr = f
	if err := cmd.Start(); err != nil {
		h.T.Fatal(err)
	}
	h.cmd = cmd
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := h.client.Get("http://prox/healthz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	b, _ := os.ReadFile(logPath)
	h.T.Fatalf("daemon did not become ready; log:\n%s", b)
}

func (h *Harness) StopDaemon() {
	if h.cmd == nil || h.cmd.Process == nil {
		return
	}
	_ = h.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		_ = h.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = h.cmd.Process.Kill()
		<-done
		h.T.Logf("daemon required SIGKILL; log:\n%s", h.log())
	}
	h.cmd = nil
	if h.logf != nil {
		h.logf.Close()
		h.logf = nil
	}
}

func (h *Harness) log() string {
	if h.logf == nil {
		return ""
	}
	b, _ := os.ReadFile(h.logf.Name())
	return string(b)
}

func (h *Harness) API(method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://prox"+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (h *Harness) CAPEM() []byte {
	h.T.Helper()
	h.caOnce.Do(func() {
		var pem []byte
		req, _ := http.NewRequest("GET", "http://prox/v1/ca", nil)
		resp, err := h.client.Do(req)
		if err == nil {
			pem, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
		}
		if len(pem) == 0 {
			pem, _ = os.ReadFile(filepath.Join(h.StateDir, "ca", "pki", "authorities", "local", "root.crt"))
		}
		if len(pem) == 0 {
			h.T.Fatalf("cannot read CA; daemon log:\n%s", h.log())
		}
		h.caPEM = pem
	})
	return h.caPEM
}

func (h *Harness) HTTPSClient(host string) *http.Client {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(h.CAPEM())
	return &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: host},
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", h.HTTPSPort))
			},
		},
	}
}

func (h *Harness) Get(host, path string) (int, string, error) {
	client := h.HTTPSClient(host)
	resp, err := client.Get("https://" + host + path)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), nil
}

func (h *Harness) WaitHTTP(host string, want int, timeout time.Duration) string {
	h.T.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		code, body, err := h.Get(host, "/")
		if err == nil && code == want {
			return body
		}
		if err != nil {
			last = err.Error()
		} else {
			last = fmt.Sprintf("status %d: %s", code, body)
		}
		time.Sleep(200 * time.Millisecond)
	}
	h.T.Fatalf("waiting for https://%s: %s\ndaemon log:\n%s", host, last, h.log())
	return ""
}

type Route struct {
	ID            int64  `json:"id"`
	Host          string `json:"host"`
	ContainerID   string `json:"container_id"`
	ContainerName string `json:"container_name"`
	Project       string `json:"project"`
	Service       string `json:"service"`
	Proto         string `json:"proto"`
	SourcePort    int    `json:"source_port"`
	GatewayPort   int    `json:"gateway_port"`
	TargetAddr    string `json:"target_addr"`
	RuleID        int64  `json:"rule_id"`
	Status        string `json:"status"`
	Collision     bool   `json:"collision"`
}

func (h *Harness) Routes() []Route {
	var routes []Route
	if err := h.API("GET", "/v1/routes", nil, &routes); err != nil {
		h.T.Fatalf("routes: %v", err)
	}
	return routes
}

func (h *Harness) WaitRoute(host, proto string, timeout time.Duration) Route {
	h.T.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, r := range h.Routes() {
			if r.Host == host && (proto == "" || r.Proto == proto) && r.Status == "up" {
				return r
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	routes, _ := json.MarshalIndent(h.Routes(), "", "  ")
	h.T.Fatalf("route %s (%s) never became up; routes:\n%s\ndaemon log:\n%s", host, proto, routes, h.log())
	return Route{}
}

func (h *Harness) AddRule(body map[string]any) {
	h.T.Helper()
	if err := h.API("POST", "/v1/rules", body, nil); err != nil {
		h.T.Fatalf("add rule: %v", err)
	}
}

func (h *Harness) Docker(args ...string) (string, error) {
	cmd := exec.Command("docker", args...)
	cmd.Env = append(os.Environ(), "DOCKER_HOST=unix://"+h.ProxySocket)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("%v: %s", err, errb.String())
	}
	return out.String(), nil
}

func (h *Harness) DockerRaw(args ...string) (string, error) {
	cmd := exec.Command("docker", args...)
	cmd.Env = append(os.Environ(), "DOCKER_HOST=unix://"+dockerSocket())
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("%v: %s", err, errb.String())
	}
	return out.String(), nil
}

func (h *Harness) MustDocker(args ...string) string {
	h.T.Helper()
	out, err := h.Docker(args...)
	if err != nil {
		h.T.Fatalf("docker %v: %v", args, err)
	}
	return out
}

func (h *Harness) MustDockerRaw(args ...string) string {
	h.T.Helper()
	out, err := h.DockerRaw(args...)
	if err != nil {
		h.T.Fatalf("docker(raw) %v: %v", args, err)
	}
	return out
}

func (h *Harness) EnsureImage(image string) {
	h.T.Helper()
	if _, err := h.DockerRaw("image", "inspect", image); err == nil {
		return
	}
	h.T.Logf("pulling %s", image)
	if out, err := h.DockerRaw("pull", image); err != nil {
		h.T.Fatalf("pull %s: %v %s", image, err, out)
	}
}

func (h *Harness) CLI(args ...string) (string, string, error) {
	cmd := exec.Command(h.Bin, args...)
	cmd.Env = h.Env
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

func (h *Harness) MustCLI(args ...string) string {
	h.T.Helper()
	out, errb, err := h.CLI(args...)
	if err != nil {
		h.T.Fatalf("gw %v: %v\nstdout: %s\nstderr: %s", args, err, out, errb)
	}
	return out
}

func (h *Harness) freeHostPort(t *testing.T) int {
	return freePort(t)
}

func (h *Harness) WaitLeaseGone(host string, timeout time.Duration) {
	h.T.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var leases []struct {
			Host string `json:"host"`
		}
		_ = h.API("GET", "/v1/leases", nil, &leases)
		found := false
		for _, l := range leases {
			if l.Host == host {
				found = true
			}
		}
		if !found {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	h.T.Fatalf("lease for %s never disappeared", host)
}

func (h *Harness) HostPortFree(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}
