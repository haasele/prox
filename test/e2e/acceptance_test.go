//go:build e2e

package e2e

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestAdHocNginx(t *testing.T) {
	h := NewHarness(t)
	h.EnsureImage("nginx:alpine")
	name := "proxe2e-nginx-" + randSuffix()
	hostPort := 3000
	if !h.HostPortFree(hostPort) {
		hostPort = freePort(t)
	}
	t.Cleanup(func() { _, _ = h.DockerRaw("rm", "-f", name) })

	h.MustDocker("run", "-d", "--rm", "--name", name, "-p", fmt.Sprintf("%d:3000", hostPort), "nginx:alpine")
	route := h.WaitRoute(name+".localhost", "http", 20*time.Second)
	if route.SourcePort != 80 {
		t.Fatalf("expected image EXPOSE port 80, got %+v", route)
	}
	if !h.HostPortFree(hostPort) {
		t.Fatalf("host port %d was bound despite the rewrite", hostPort)
	}
	body := h.WaitHTTP(name+".localhost", 200, 20*time.Second)
	if !strings.Contains(body, "Welcome to nginx") {
		t.Fatalf("unexpected body: %s", body[:min(200, len(body))])
	}
	// The dashboard is served through the same Caddy instance.
	dash := h.WaitHTTP("prox.localhost", 200, 20*time.Second)
	if !strings.Contains(dash, `id="applications"`) || !strings.Contains(dash, `id="rule-form"`) ||
		!strings.Contains(dash, `data-tab="activity"`) || !strings.Contains(dash, "<dialog") {
		t.Fatalf("dashboard body missing tabs, dialog, application table or rule form: %s", dash[:min(160, len(dash))])
	}

	// The proxy's list/inspect views must present the original binding.
	out := h.MustDocker("inspect", name, "--format", "{{json .HostConfig.PortBindings}}")
	if !strings.Contains(out, "3000/tcp") {
		t.Fatalf("inspect did not restore original ports: %s", out)
	}
	raw, err := h.DockerRaw("inspect", name, "--format", "{{json .HostConfig.PortBindings}}")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "3000/tcp") {
		t.Fatalf("raw docker still has port bindings: %s", raw)
	}
}

func TestOriginalPortHint(t *testing.T) {
	h := NewHarnessWithOriginalHints(t)
	h.EnsureImage("nginx:alpine")
	name := "proxe2e-hint-" + randSuffix()
	hostPort := freePort(t)
	t.Cleanup(func() { _, _ = h.DockerRaw("rm", "-f", name) })

	h.MustDocker("run", "-d", "--rm", "--name", name, "-p", fmt.Sprintf("%d:3000", hostPort), "nginx:alpine")
	h.WaitRoute(name+".localhost", "http", 20*time.Second)
	target := fmt.Sprintf("https://%s.localhost:%d", name, h.HTTPSPort)

	client := &http.Client{Timeout: 2 * time.Second}
	hintURL := fmt.Sprintf("http://127.0.0.1:%d/", hostPort)
	var body string
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(hintURL)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			body = string(b)
			if strings.Contains(body, "prox routes this application") {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, want := range []string{name, target, "prox routes this application"} {
		if !strings.Contains(body, want) {
			t.Fatalf("original port %d body missing %q: %s", hostPort, want, body)
		}
	}
	if strings.Contains(body, "Welcome to nginx") {
		t.Fatalf("original port served the application instead of the prox hint")
	}

	// The hint must go away with the route.
	if _, err := h.DockerRaw("rm", "-f", name); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(20 * time.Second)
	for {
		resp, err := client.Get(hintURL)
		if err == nil {
			resp.Body.Close()
			if time.Now().After(deadline) {
				t.Fatal("original-port hint kept listening after the container was removed")
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		break
	}
}

func TestComposeTwoProjectsAndStability(t *testing.T) {
	h := NewHarness(t)
	h.EnsureImage("nginx:alpine")
	sfx := randSuffix()
	projA, projB := "proxe2ea"+sfx[:4], "proxe2eb"+sfx[:4]
	dirA, dirB := t.TempDir(), t.TempDir()
	hostPort := freePort(t)
	writeCompose(t, dirA, projA, "nginx:alpine", fmt.Sprintf("%d:80", hostPort), "443:443")
	writeCompose(t, dirB, projB, "nginx:alpine", fmt.Sprintf("%d:80", hostPort), "443:443")
	t.Cleanup(func() { composeDown(t, h, dirA, projA) })
	t.Cleanup(func() { composeDown(t, h, dirB, projB) })

	composeUp(t, h, dirA, projA)
	composeUp(t, h, dirB, projB)

	idA := strings.TrimSpace(h.MustDockerRaw("inspect", projA+"-web-1", "--format", "{{.Id}}"))
	idB := strings.TrimSpace(h.MustDockerRaw("inspect", projB+"-web-1", "--format", "{{.Id}}"))

	h.WaitRoute("web."+projA+".localhost", "http", 20*time.Second)
	h.WaitRoute("web."+projB+".localhost", "http", 20*time.Second)
	h.WaitHTTP("web."+projA+".localhost", 200, 20*time.Second)
	h.WaitHTTP("web."+projB+".localhost", 200, 20*time.Second)

	// A second up must be a no-op and must not recreate containers.
	composeUp(t, h, dirA, projA)
	composeUp(t, h, dirB, projB)
	idA2 := strings.TrimSpace(h.MustDockerRaw("inspect", projA+"-web-1", "--format", "{{.Id}}"))
	idB2 := strings.TrimSpace(h.MustDockerRaw("inspect", projB+"-web-1", "--format", "{{.Id}}"))
	if idA != idA2 || idB != idB2 {
		t.Fatalf("compose recreated containers: %s/%s -> %s/%s", idA, idB, idA2, idB2)
	}
	if !h.HostPortFree(hostPort) {
		t.Fatalf("host port %d is bound despite two compose projects mapping it", hostPort)
	}
}

func TestPostgresGatewayAliases(t *testing.T) {
	h := NewHarness(t)
	h.EnsureImage("postgres:17-alpine")
	project := "proxe2epg" + randSuffix()[:4]
	gwPort := 5432
	if !h.HostPortFree(gwPort) {
		gwPort = freePort(t)
	}
	out := h.MustCLI("allow", "postgres", "--container-port", "5432", "--gateway-port", fmt.Sprint(gwPort), "--resolve", "database", "--project", project)
	if !strings.Contains(out, fmt.Sprintf("database.localhost:%d", gwPort)) {
		t.Fatalf("unexpected allow output: %s", out)
	}
	name := "proxe2e-pg-" + randSuffix()
	t.Cleanup(func() { _, _ = h.DockerRaw("rm", "-f", name) })
	h.MustDocker("run", "-d", "--rm", "--name", name,
		"-l", "com.docker.compose.project="+project,
		"-l", "com.docker.compose.service=postgres",
		"-p", fmt.Sprintf("%d:5432", freePort(t)),
		"-e", "POSTGRES_PASSWORD=pass",
		"postgres:17-alpine")
	route := h.WaitRoute("database.localhost", "tcp", 20*time.Second)
	if route.GatewayPort != gwPort || route.SourcePort != 5432 {
		t.Fatalf("route: %+v", route)
	}
	waitPostgresSSL(t, h, gwPort)

	// The canonical --source/--target spelling must behave identically.
	aliasProject := project + "b"
	gw2 := freePort(t)
	h.MustCLI("allow", "postgres", "--source", "5432", "--target", fmt.Sprint(gw2), "--resolve", "database2", "--project", aliasProject)
	name2 := "proxe2e-pg2-" + randSuffix()
	t.Cleanup(func() { _, _ = h.DockerRaw("rm", "-f", name2) })
	h.MustDocker("run", "-d", "--rm", "--name", name2,
		"-l", "com.docker.compose.project="+aliasProject,
		"-l", "com.docker.compose.service=postgres",
		"-p", fmt.Sprintf("%d:5432", freePort(t)),
		"-e", "POSTGRES_PASSWORD=pass",
		"postgres:17-alpine")
	route2 := h.WaitRoute("database2.localhost", "tcp", 20*time.Second)
	if route2.GatewayPort != gw2 || route2.SourcePort != 5432 {
		t.Fatalf("canonical route: %+v", route2)
	}
	waitPostgresSSL(t, h, gw2)

	var rules []struct {
		Name    string `json:"name"`
		Project string `json:"project"`
		Source  int    `json:"source"`
		Target  int    `json:"target"`
		Resolve string `json:"resolve"`
		Proto   string `json:"proto"`
	}
	if err := h.API("GET", "/v1/rules", nil, &rules); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rules {
		if r.Project == aliasProject {
			found = true
			if r.Source != 5432 || r.Target != gw2 || r.Resolve != "database2.localhost" || r.Proto != "tcp" {
				t.Fatalf("alias rule: %+v", r)
			}
		}
	}
	if !found {
		t.Fatal("alias rule missing")
	}
}

func waitPostgresSSL(t *testing.T, h *Harness, gwPort int) {
	t.Helper()
	// PostgreSQL SSLRequest: len=8, code=80877103. Any single-byte reply proves
	// the gateway reached Postgres (S=SSL supported, N=not configured).
	msg := []byte{0, 0, 0, 8, 4, 210, 22, 47}
	var lastErr error
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", gwPort), 5*time.Second)
		if err != nil {
			lastErr = err
			time.Sleep(300 * time.Millisecond)
			continue
		}
		_, _ = conn.Write(msg)
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 1)
		_, err = conn.Read(buf)
		conn.Close()
		if err == nil {
			if buf[0] != 'S' && buf[0] != 'N' {
				t.Fatalf("unexpected postgres reply %q", buf)
			}
			return
		}
		lastErr = err
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("postgres gateway read: %v\nlog:\n%s", lastErr, h.log())
}

func TestPostgresSNI(t *testing.T) {
	h := NewHarness(t)
	// A protocol without a routable SNI name must produce the documented error.
	if out, errb, err := h.CLI("allow", "redis", "--target", "443"); err == nil {
		t.Fatalf("redis --target 443 unexpectedly succeeded: %s", out)
	} else if !strings.Contains(errb+out, "SNI") {
		t.Fatalf("redis SNI error missing: stdout=%s stderr=%s", out, errb)
	}
	h.EnsureImage("postgres:17-alpine")
	h.EnsureImage("alpine:3.22")
	project := "proxe2esni" + randSuffix()[:4]
	h.MustCLI("allow", "postgres", "--source", "5432", "--target", "443", "--resolve", "dbsni", "--project", project)

	certDir := t.TempDir()
	writeServerCert(t, certDir, "prox-e2e-postgres")
	vol := "proxe2e-certs-" + randSuffix()
	h.MustDockerRaw("volume", "create", vol)
	t.Cleanup(func() { _, _ = h.DockerRaw("volume", "rm", "-f", vol) })
	pgUID := strings.TrimSpace(h.MustDockerRaw("run", "--rm", "--entrypoint", "sh", "postgres:17-alpine", "-c", "id -u postgres"))
	h.MustDockerRaw("run", "--rm", "-v", vol+":/certs", "-v", certDir+":/src:ro", "alpine:3.22",
		"sh", "-c", fmt.Sprintf("cp /src/server.crt /src/server.key /certs/ && chown %s:%s /certs/server.crt /certs/server.key && chmod 600 /certs/server.key", pgUID, pgUID))

	name := "proxe2e-sni-" + randSuffix()
	t.Cleanup(func() { _, _ = h.DockerRaw("rm", "-f", name) })
	h.MustDocker("run", "-d", "--rm", "--name", name,
		"-l", "com.docker.compose.project="+project,
		"-l", "com.docker.compose.service=postgres",
		"-p", fmt.Sprintf("%d:5432", freePort(t)),
		"-e", "POSTGRES_PASSWORD=pass",
		"-v", vol+":/certs:ro",
		"postgres:17-alpine",
		"-c", "ssl=on", "-c", "ssl_cert_file=/certs/server.crt", "-c", "ssl_key_file=/certs/server.key")
	h.WaitRoute("dbsni.localhost", "sni", 30*time.Second)

	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		dialer := &net.Dialer{Timeout: 3 * time.Second}
		conn, err := tls.DialWithDialer(dialer, "tcp", fmt.Sprintf("127.0.0.1:%d", h.HTTPSPort), &tls.Config{
			ServerName: "dbsni.localhost", InsecureSkipVerify: true,
		})
		if err == nil {
			certs := conn.ConnectionState().PeerCertificates
			conn.Close()
			if len(certs) > 0 && certs[0].Subject.CommonName == "prox-e2e-postgres" {
				psqlDirect(t, h, "dbsni.localhost", h.HTTPSPort)
				return // SNI passthrough reached the Postgres TLS listener.
			}
			lastErr = fmt.Errorf("handshake reached the wrong TLS server (subject %v)", certs[0].Subject)
		} else {
			lastErr = err
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("Postgres SNI handshake never reached the container: %v\nlog:\n%s", lastErr, h.log())
}

func psqlDirect(t *testing.T, h *Harness, host string, port int) string {
	t.Helper()
	h.EnsureImage("postgres:17-alpine")
	dsn := fmt.Sprintf("postgresql://postgres:pass@%s:%d/postgres?sslmode=require&sslnegotiation=direct&hostaddr=127.0.0.1", host, port)
	deadline := time.Now().Add(60 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		out, err := h.DockerRaw("run", "--rm", "--network", "host", "postgres:17-alpine", "psql", dsn, "-tAc", "select 45")
		if err == nil && strings.Contains(out, "45") {
			return out
		}
		last = out
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("psql sslnegotiation=direct failed: %s\nlog:\n%s", last, h.log())
	return ""
}

func TestRunVite(t *testing.T) {
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm not installed")
	}
	h := NewHarness(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "package.json"), `{
  "name": "prox-e2e-vite",
  "private": true,
  "type": "module",
  "scripts": {"dev": "npx --yes vite@7"},
  "devDependencies": {"vite": "^7.0.0"}
}`)
	writeFile(t, filepath.Join(dir, "vite.config.js"), `export default { server: { allowedHosts: true } }`)
	writeFile(t, filepath.Join(dir, "index.html"), `<!doctype html><html><body><div id="app">prox vite fixture</div><script type="module" src="/main.js"></script></body></html>`)
	writeFile(t, filepath.Join(dir, "main.js"), `import './style.css'; document.querySelector('#app').textContent='vite hmr ready';`)
	writeFile(t, filepath.Join(dir, "style.css"), `body{background:#123}`)
	host := filepath.Base(dir) + ".localhost"

	logPath := filepath.Join(t.TempDir(), "gw-run.log")
	lf, _ := os.Create(logPath)
	cmd := exec.Command(h.Bin, "run", "--", "npm", "run", "dev")
	cmd.Dir = dir
	cmd.Env = h.Env
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	killed := false
	kill := func() {
		if killed {
			return
		}
		killed = true
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
	}
	t.Cleanup(kill)
	lf.Close()

	// Wait for the lease and Vite's HTTP endpoint.
	var leases []struct {
		Host string `json:"host"`
	}
	deadline := time.Now().Add(120 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		if err := h.API("GET", "/v1/leases", nil, &leases); err == nil {
			for _, l := range leases {
				if l.Host == host {
					if _, _, err := h.Get(host, "/"); err == nil {
						ready = true
					}
				}
			}
		}
		if ready {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !ready {
		b, _ := os.ReadFile(logPath)
		t.Fatalf("Vite never became reachable; log:\n%s\ndaemon:\n%s", b, h.log())
	}
	body := h.WaitHTTP(host, 200, 20*time.Second)
	if !strings.Contains(body, "vite fixture") {
		t.Fatalf("body: %s", body)
	}
	if st, _, err := h.Get(host, "/@vite/client"); err != nil || st != 200 {
		t.Fatalf("@vite/client status=%d err=%v", st, err)
	}

	// HMR WebSocket through Caddy.
	hmrOK := waitHMR(t, host, h.HTTPSPort)
	if !hmrOK {
		t.Fatal("Vite HMR websocket did not connect")
	}

	kill()
	h.WaitLeaseGone(host, 15*time.Second)
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		found := false
		for _, r := range h.Routes() {
			if r.Host == host {
				found = true
			}
		}
		if !found {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("route was not removed after prox run exited")
}

func waitHMR(t *testing.T, host string, port int) bool {
	t.Helper()
	pool := x509.NewCertPool()
	// The caller's harness CA is not passed; rely on InsecureSkipVerify since
	// we are testing transport, then assert the 101 response.
	_ = pool
	var lastErr error
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := tls.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port), &tls.Config{
			ServerName: host, InsecureSkipVerify: true,
		})
		if err != nil {
			lastErr = err
			time.Sleep(200 * time.Millisecond)
			continue
		}
		key := make([]byte, 16)
		_, _ = rand.Read(key)
		req := fmt.Sprintf("GET / HTTP/1.1\r\nHost: %s:%d\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Protocol: vite-hmr\r\n\r\n",
			host, port, base64.StdEncoding.EncodeToString(key))
		_, _ = conn.Write([]byte(req))
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		buf := make([]byte, 4096)
		n, err := conn.Read(buf)
		if err != nil {
			lastErr = err
			conn.Close()
			time.Sleep(200 * time.Millisecond)
			continue
		}
		resp := string(buf[:n])
		if !strings.Contains(resp, "101") {
			lastErr = fmt.Errorf("response: %s", resp)
			conn.Close()
			time.Sleep(200 * time.Millisecond)
			continue
		}
		n, _ = conn.Read(buf)
		data := append([]byte(resp), buf[:n]...)
		conn.Close()
		return bytes.Contains(data, []byte(`"type":"connected"`))
	}
	t.Logf("HMR last error: %v", lastErr)
	return false
}

func TestRunNode(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	h := NewHarness(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "server.js"), `const http=require('http');
http.createServer((req,res)=>{res.writeHead(200,{'content-type':'text/plain'});res.end('node run works\n');})
  .listen(Number(process.env.PORT), process.env.HOST||'127.0.0.1');
`)
	host := filepath.Base(dir) + ".localhost"
	logPath := filepath.Join(t.TempDir(), "gw-run.log")
	lf, _ := os.Create(logPath)
	cmd := exec.Command(h.Bin, "run", "--", "node", "server.js")
	cmd.Dir = dir
	cmd.Env = h.Env
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	kill := func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
	}
	defer kill()
	lf.Close()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if code, body, err := h.Get(host, "/"); err == nil && code == 200 && strings.Contains(body, "node run works") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	body := h.WaitHTTP(host, 200, 20*time.Second)
	if !strings.Contains(body, "node run works") {
		t.Fatalf("body: %q", body)
	}
	kill()
	h.WaitLeaseGone(host, 15*time.Second)
}

func TestComposeCheckPredicts(t *testing.T) {
	h := NewHarness(t)
	h.EnsureImage("nginx:alpine")
	h.EnsureImage("postgres:17-alpine")
	h.EnsureImage("alpine:3.22")
	project := "proxe2echk" + randSuffix()[:4]
	gw := freePort(t)
	h.MustCLI("allow", "postgres", "--source", "5432", "--target", fmt.Sprint(gw), "--project", project)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "compose.yml"), fmt.Sprintf(`
name: %s
services:
  web:
    image: nginx:alpine
    ports: ["80:80"]
  db:
    image: postgres:17-alpine
    environment:
      POSTGRES_PASSWORD: pass
    ports: ["5432:5432"]
    depends_on:
      - web
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U postgres"]
      interval: 2s
      timeout: 3s
      retries: 30
  worker:
    image: alpine:3.22
    command: ["sleep", "600"]
`, project))
	t.Cleanup(func() { composeDown(t, h, dir, project) })

	out := h.MustCLI("check", "--project", project, "-j", filepath.Join(dir, "compose.yml"))
	var resp struct {
		Services []struct {
			Service     string `json:"service"`
			Status      string `json:"status"`
			Host        string `json:"host"`
			Action      string `json:"action"`
			SourcePort  int    `json:"source_port"`
			GatewayPort int    `json:"gateway_port"`
		} `json:"services"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("check json: %v\n%s", err, out)
	}
	if len(resp.Services) != 3 {
		t.Fatalf("services: %+v", resp.Services)
	}
	byName := map[string]int{}
	for i, s := range resp.Services {
		byName[s.Service] = i
	}
	if resp.Services[byName["web"]].Status != "routed-http" || resp.Services[byName["web"]].Host != "web."+project+".localhost" {
		t.Fatalf("web prediction: %+v", resp.Services[byName["web"]])
	}
	if resp.Services[byName["db"]].Status != "routed-tcp" || resp.Services[byName["db"]].GatewayPort != gw {
		t.Fatalf("db prediction: %+v", resp.Services[byName["db"]])
	}
	if resp.Services[byName["worker"]].Status != "routed-http" || resp.Services[byName["worker"]].SourcePort != 0 {
		t.Fatalf("worker prediction: %+v", resp.Services[byName["worker"]])
	}

	dbHost := resp.Services[byName["db"]].Host
	composeUp(t, h, dir, project)
	h.WaitRoute("web."+project+".localhost", "http", 30*time.Second)
	h.WaitRoute(dbHost, "tcp", 30*time.Second)
	routes := h.Routes()
	hosts := map[string]bool{}
	for _, r := range routes {
		hosts[r.Host] = true
	}
	for _, s := range resp.Services {
		if s.Host != "" && !hosts[s.Host] {
			t.Fatalf("predicted host %s missing from live routes: %+v", s.Host, routes)
		}
	}
}

func TestPublishAndDeny(t *testing.T) {
	h := NewHarness(t)
	h.EnsureImage("nginx:alpine")
	h.EnsureImage("alpine:3.22")
	h.AddRule(map[string]any{"name": "nginx", "action": "publish"})
	name := "proxe2e-pub-" + randSuffix()
	t.Cleanup(func() { _, _ = h.DockerRaw("rm", "-f", name) })
	hostPort := freePort(t)
	h.MustDocker("run", "-d", "--rm", "--name", name, "-p", fmt.Sprintf("%d:80", hostPort), "nginx:alpine")
	deadline := time.Now().Add(10 * time.Second)
	bound := false
	for time.Now().Before(deadline) {
		if !h.HostPortFree(hostPort) {
			bound = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !bound {
		t.Fatalf("publish rule did not keep host port %d", hostPort)
	}
	for _, r := range h.Routes() {
		if r.ContainerName == name {
			t.Fatalf("published container should not have a route: %+v", r)
		}
	}

	// Deny policy surface.
	h.MustCLI("policy", "set", "deny")
	denyPort := freePort(t)
	_, derr := h.Docker("run", "--rm", "-p", fmt.Sprintf("%d:80", denyPort), "alpine:3.22", "true")
	if derr == nil {
		t.Fatal("deny policy allowed a port-binding create")
	}
	if !strings.Contains(derr.Error(), "policy is 'deny'") || !strings.Contains(derr.Error(), "prox allow") {
		t.Fatalf("deny error = %v", derr)
	}
}

func TestCollision(t *testing.T) {
	h := NewHarness(t)
	h.EnsureImage("nginx:alpine")
	h.AddRule(map[string]any{"name": "web", "source": 80, "resolve": "app", "action": "allow"})
	sfx := randSuffix()
	projA, projB := "proxe2eca"+sfx[:4], "proxe2ecb"+sfx[:4]
	dirA, dirB := t.TempDir(), t.TempDir()
	hostPort := freePort(t)
	writeCompose(t, dirA, projA, "nginx:alpine", fmt.Sprintf("%d:80", hostPort), "443:443")
	writeCompose(t, dirB, projB, "nginx:alpine", fmt.Sprintf("%d:80", hostPort), "443:443")
	t.Cleanup(func() { composeDown(t, h, dirA, projA) })
	t.Cleanup(func() { composeDown(t, h, dirB, projB) })
	composeUp(t, h, dirA, projA)
	composeUp(t, h, dirB, projB)
	h.WaitRoute("app.localhost", "http", 20*time.Second)
	h.WaitRoute("app."+projB+".localhost", "http", 20*time.Second)
	h.WaitHTTP("app.localhost", 200, 20*time.Second)
	h.WaitHTTP("app."+projB+".localhost", 200, 20*time.Second)
	var view struct {
		Warnings []string `json:"warnings"`
	}
	if err := h.API("GET", "/v1/containers", nil, &view); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(view.Warnings, "\n")
	if !strings.Contains(joined, "collision") {
		t.Fatalf("no collision warning: %v", view.Warnings)
	}
}

func TestStoppedContainerReturns503(t *testing.T) {
	h := NewHarness(t)
	h.EnsureImage("nginx:alpine")
	name := "proxe2e-503-" + randSuffix()
	t.Cleanup(func() { _, _ = h.DockerRaw("rm", "-f", name) })
	// Intentionally no --rm: a stopped container keeps its route, which must
	// turn into the friendly 503 page.
	h.MustDocker("run", "-d", "--name", name, "-p", fmt.Sprintf("%d:80", freePort(t)), "nginx:alpine")
	h.WaitRoute(name+".localhost", "http", 20*time.Second)
	h.WaitHTTP(name+".localhost", 200, 20*time.Second)

	h.MustDocker("stop", name)
	deadline := time.Now().Add(30 * time.Second)
	last := ""
	for time.Now().Before(deadline) {
		code, body, err := h.Get(name+".localhost", "/")
		if err == nil && code == 503 && strings.Contains(body, "prox dashboard") {
			return
		}
		if err != nil {
			last = err.Error()
		} else {
			last = fmt.Sprintf("status %d body %q", code, body)
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("stopped container did not yield the friendly 503 page (%s); log:\n%s", last, h.log())
}

func TestDaemonRestart(t *testing.T) {
	h := NewHarness(t)
	h.EnsureImage("nginx:alpine")
	name := "proxe2e-restart-" + randSuffix()
	t.Cleanup(func() { _, _ = h.DockerRaw("rm", "-f", name) })
	h.MustDocker("run", "-d", "--rm", "--name", name, "-p", fmt.Sprintf("%d:80", freePort(t)), "nginx:alpine")
	route := h.WaitRoute(name+".localhost", "http", 20*time.Second)
	h.WaitHTTP(name+".localhost", 200, 20*time.Second)

	h.StopDaemon()
	h.StartDaemon()
	// The CA and routes must survive; the route comes back after reconcile.
	h.WaitRoute(route.Host, "http", 30*time.Second)
	h.WaitHTTP(name+".localhost", 200, 20*time.Second)
	routes := h.Routes()
	seen := map[string]bool{}
	for _, r := range routes {
		if seen[r.Host+"|"+r.Proto] {
			t.Fatalf("duplicate route after restart: %+v", r)
		}
		seen[r.Host+"|"+r.Proto] = true
	}
}

// TestDockerRestart is opt-in because restarting the shared Docker daemon is
// disruptive. It is run in its own CI job.
func TestDockerRestart(t *testing.T) {
	if os.Getenv("PROX_E2E_DOCKER_RESTART") != "1" {
		t.Skip("set PROX_E2E_DOCKER_RESTART=1 to restart dockerd (requires sudo)")
	}
	h := NewHarness(t)
	h.EnsureImage("nginx:alpine")
	name := "proxe2e-dockerrestart-" + randSuffix()
	t.Cleanup(func() { _, _ = h.DockerRaw("rm", "-f", name) })
	h.MustDocker("run", "-d", "--name", name, "-p", fmt.Sprintf("%d:80", freePort(t)), "nginx:alpine")
	route := h.WaitRoute(name+".localhost", "http", 30*time.Second)
	h.WaitHTTP(name+".localhost", 200, 30*time.Second)

	restarted := false
	for _, args := range [][]string{
		{"sudo", "-n", "systemctl", "restart", "docker"},
		{"sudo", "-n", "service", "docker", "restart"},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err == nil {
			restarted = true
			break
		} else {
			t.Logf("%v: %v %s", args, err, out)
		}
	}
	if !restarted {
		t.Skip("cannot restart dockerd without passwordless sudo")
	}

	// Wait for the raw daemon to come back.
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := h.DockerRaw("version", "--format", "{{.Server.Version}}"); err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if _, err := h.DockerRaw("version", "--format", "{{.Server.Version}}"); err != nil {
		t.Fatalf("Docker did not come back: %v", err)
	}

	h.WaitRoute(route.Host, "http", 120*time.Second)
	h.WaitHTTP(name+".localhost", 200, 60*time.Second)
	seen := map[string]bool{}
	for _, r := range h.Routes() {
		if seen[r.Host+"|"+r.Proto] {
			t.Fatalf("duplicate route after Docker restart: %+v", r)
		}
		seen[r.Host+"|"+r.Proto] = true
	}
}

// ---- helpers ----

func writeCompose(t *testing.T, dir, project, image string, ports ...string) {
	t.Helper()
	var b strings.Builder
	for _, p := range ports {
		fmt.Fprintf(&b, "      - %q\n", p)
	}
	writeFile(t, filepath.Join(dir, "compose.yml"), fmt.Sprintf(`
name: %s
services:
  web:
    image: %s
    ports:
%s`, project, image, b.String()))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func composeUp(t *testing.T, h *Harness, dir, project string) {
	t.Helper()
	cmd := exec.Command("docker", "compose", "-f", filepath.Join(dir, "compose.yml"), "-p", project, "up", "-d")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "DOCKER_HOST=unix://"+h.ProxySocket)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("compose up: %v\n%s\nlog:\n%s", err, out, h.log())
	}
}

func composeDown(t *testing.T, h *Harness, dir, project string) {
	cmd := exec.Command("docker", "compose", "-f", filepath.Join(dir, "compose.yml"), "-p", project, "down", "-v", "--remove-orphans")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "DOCKER_HOST=unix://"+h.ProxySocket)
	_, _ = cmd.CombinedOutput()
}

func writeServerCert(t *testing.T, dir, cn string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"dbsni.localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "server.crt"), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
	writeFile(t, filepath.Join(dir, "server.key"), string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
