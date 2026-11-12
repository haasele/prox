package dockerproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/haasele/prox/internal/backend"
	"github.com/haasele/prox/internal/dockerclient"
	"github.com/haasele/prox/internal/policy"
	"github.com/haasele/prox/internal/registry"
	"github.com/haasele/prox/profiles"
)

type fakeDocker struct {
	dir      string
	sock     string
	srv      *http.Server
	mu       sync.Mutex
	creates  []map[string]any
	headers  []http.Header
	inspect  map[string]any
	listJSON []any
}

func newFakeDocker(t *testing.T) *fakeDocker {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "docker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeDocker{dir: dir, sock: sock}
	f.inspect = map[string]any{
		"Id":   "deadbeef",
		"Name": "/web-1",
		"Config": map[string]any{
			"Image":        "nginx:latest",
			"ExposedPorts": map[string]any{"3000/tcp": map[string]any{}},
			"Labels": map[string]any{
				LabelManaged: "true", LabelHost: "web.1.localhost", LabelProto: "http", LabelPort: "3000",
			},
		},
		"HostConfig": map[string]any{
			"NetworkMode":     "gw",
			"PublishAllPorts": false,
		},
		"NetworkSettings": map[string]any{
			"Networks": map[string]any{"gw": map[string]any{"IPAddress": "172.18.0.2"}},
			"Ports":    map[string]any{},
		},
	}
	f.listJSON = []any{map[string]any{
		"Id": "deadbeef", "Names": []any{"/web-1"}, "Image": "nginx:latest",
		"Ports": []any{}, "Labels": map[string]any{},
	}}
	mux := http.NewServeMux()
	mux.HandleFunc("/", f.handle)
	f.srv = &http.Server{Handler: mux}
	go f.srv.Serve(ln)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		f.srv.Shutdown(ctx)
	})
	return f
}

func (f *fakeDocker) handle(w http.ResponseWriter, r *http.Request) {
	path := versionPrefix.ReplaceAllString(r.URL.Path, "")
	f.mu.Lock()
	f.headers = append(f.headers, r.Header.Clone())
	f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && path == "/_ping":
		w.Header().Set("API-Version", "1.45")
		_, _ = w.Write([]byte("OK"))
	case r.Method == http.MethodGet && path == "/networks/gw":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Id": "netgw", "Name": "gw", "Driver": "bridge",
			"IPAM": map[string]any{"Config": []any{map[string]any{"Subnet": "172.18.0.0/16", "Gateway": "172.18.0.1"}}},
		})
	case r.Method == http.MethodPost && path == "/containers/create":
		b, _ := io.ReadAll(r.Body)
		var obj map[string]any
		_ = json.Unmarshal(b, &obj)
		f.mu.Lock()
		f.creates = append(f.creates, obj)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"Id":"deadbeef","Warnings":[]}`))
	case r.Method == http.MethodGet && path == "/containers/deadbeef/json":
		w.Header().Set("Content-Type", "application/json")
		f.mu.Lock()
		_ = json.NewEncoder(w).Encode(f.inspect)
		f.mu.Unlock()
	case r.Method == http.MethodGet && path == "/containers/web-1/json":
		w.Header().Set("Content-Type", "application/json")
		f.mu.Lock()
		_ = json.NewEncoder(w).Encode(f.inspect)
		f.mu.Unlock()
	case r.Method == http.MethodGet && path == "/containers/json":
		w.Header().Set("Content-Type", "application/json")
		f.mu.Lock()
		_ = json.NewEncoder(w).Encode(f.listJSON)
		f.mu.Unlock()
	case strings.HasSuffix(path, "/start") && strings.Contains(r.Header.Get("Connection"), "Upgrade"):
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijacker", 500)
			return
		}
		conn, brw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = brw.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		_ = brw.Flush()
		// Echo two messages so the test can verify both directions.
		for i := 0; i < 2; i++ {
			line, err := brw.ReadString('\n')
			if err != nil {
				return
			}
			_, _ = brw.WriteString("echo:" + line)
			_ = brw.Flush()
		}
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/connect"):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"endpoint with name web already exists in network gw"}`))
	case strings.HasSuffix(path, "/logs"):
		w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("first line\n"))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		time.Sleep(20 * time.Millisecond)
		_, _ = w.Write([]byte("second line\n"))
	default:
		http.Error(w, `{"message":"not found"}`, 404)
	}
}

func setupProxy(t *testing.T, fake *fakeDocker, mutate func(*registry.Registry, *policy.Engine)) (*Proxy, *registry.Registry, string) {
	t.Helper()
	reg, err := registry.Open(filepath.Join(t.TempDir(), "prox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	set, err := profiles.LoadBuiltins()
	if err != nil {
		t.Fatal(err)
	}
	eng := policy.New(reg, set, "localhost")
	if mutate != nil {
		mutate(reg, eng)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	canBind := func(p int) bool { return p > 1024 }
	p := New(Config{
		RealSocket:  fake.sock,
		ProxySocket: filepath.Join(t.TempDir(), "proxy.sock"),
		Domain:      "localhost",
		HTTPSPort:   443,
		SignKey:     []byte("test-key"),
	}, reg, eng, backend.Native{}, dockerclient.New(fake.sock), NewAllocator(reg, 25000, 25010, canBind, log), log)
	if err := p.EnsureNetwork(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		p.Close(ctx)
	})
	return p, reg, p.cfg.ProxySocket
}

func unixHTTPClient(sock string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
}

func createRequest(t *testing.T, sock, name string, body map[string]any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, "http://docker/v1.45/containers/create?name="+name, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := unixHTTPClient(sock).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func nginxCreate() map[string]any {
	return map[string]any{
		"Image":        "nginx:latest",
		"ExposedPorts": map[string]any{"3000/tcp": map[string]any{}},
		"Config": map[string]any{
			"Image": "nginx:latest",
		},
		"HostConfig": map[string]any{
			"PortBindings": map[string]any{
				"3000/tcp": []any{map[string]any{"HostIp": "", "HostPort": "3000"}},
			},
			"PublishAllPorts": false,
		},
	}
}

func TestCreateRewriteAndInspect(t *testing.T) {
	fake := newFakeDocker(t)
	_, reg, sock := setupProxy(t, fake, nil)
	resp := createRequest(t, sock, "web-1", nginxCreate())
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create status %d: %s", resp.StatusCode, b)
	}
	var created struct {
		Warnings []string `json:"Warnings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if len(created.Warnings) != 1 || !strings.Contains(created.Warnings[0], "port 3000/tcp routed by prox -> https://web-1.localhost") {
		t.Fatalf("create warnings = %v", created.Warnings)
	}
	if len(fake.creates) != 1 {
		t.Fatalf("upstream saw %d creates", len(fake.creates))
	}
	got := fake.creates[0]
	hc := got["HostConfig"].(map[string]any)
	if _, ok := hc["PortBindings"]; ok {
		t.Fatalf("PortBindings should be stripped: %+v", hc)
	}
	if hc["PublishAllPorts"] != false {
		t.Fatalf("PublishAllPorts should be false: %+v", hc)
	}
	endpoints := got["NetworkingConfig"].(map[string]any)["EndpointsConfig"].(map[string]any)
	if _, ok := endpoints["gw"]; !ok {
		t.Fatalf("gw network not attached: %+v", endpoints)
	}
	labels := got["Labels"].(map[string]any)
	for k, want := range map[string]string{LabelManaged: "true", LabelHost: "web-1.localhost", LabelPort: "3000", LabelProto: "http"} {
		if labels[k] != want {
			t.Fatalf("label %s = %v, want %s (labels %+v)", k, labels[k], want, labels)
		}
	}
	signed := false
	for _, h := range fake.headers {
		if h.Get(SignatureHeader) != "" {
			signed = true
		}
	}
	if !signed {
		t.Fatal("missing signature header")
	}
	routes, _ := reg.ListRoutes()
	if len(routes) != 1 || routes[0].Host != "web-1.localhost" || routes[0].Status != "pending" {
		t.Fatalf("routes: %+v", routes)
	}
	events, _ := reg.ListEvents(0, 10)
	var routed *registry.Event
	for i := range events {
		if events[i].Kind == "rewrite" {
			routed = &events[i]
			break
		}
	}
	if routed == nil {
		t.Fatalf("no rewrite event: %+v", events)
	}
	if !strings.Contains(routed.Message, "port 3000/tcp routed by prox -> https://web-1.localhost") {
		t.Fatalf("rewrite message = %q", routed.Message)
	}
	if routed.Data["url"] != "https://web-1.localhost" || routed.Data["source_port"] != float64(3000) {
		t.Fatalf("rewrite event data = %+v", routed.Data)
	}

	// Inspect must present the original bindings to Compose.
	req, _ := http.NewRequest(http.MethodGet, "http://docker/v1.45/containers/deadbeef/json", nil)
	ir, err := unixHTTPClient(sock).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer ir.Body.Close()
	var inspect map[string]any
	if err := json.NewDecoder(ir.Body).Decode(&inspect); err != nil {
		t.Fatal(err)
	}
	pb := inspect["HostConfig"].(map[string]any)["PortBindings"].(map[string]any)
	if len(pb["3000/tcp"].([]any)) != 1 {
		t.Fatalf("inspect PortBindings not restored: %+v", pb)
	}
	ports := inspect["NetworkSettings"].(map[string]any)["Ports"].(map[string]any)
	if len(ports["3000/tcp"].([]any)) != 1 {
		t.Fatalf("inspect NetworkSettings.Ports not restored: %+v", ports)
	}

	// List must present the original binding too.
	req, _ = http.NewRequest(http.MethodGet, "http://docker/containers/json", nil)
	lr, err := unixHTTPClient(sock).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer lr.Body.Close()
	var list []map[string]any
	if err := json.NewDecoder(lr.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list[0]["Ports"].([]any)) == 0 {
		t.Fatalf("list ports not restored: %+v", list[0])
	}
}

func TestCreateUsesComposeLabels(t *testing.T) {
	fake := newFakeDocker(t)
	_, reg, sock := setupProxy(t, fake, nil)
	body := nginxCreate()
	body["Labels"] = map[string]any{
		"com.docker.compose.project": "shop",
		"com.docker.compose.service": "web",
	}
	resp := createRequest(t, sock, "shop-web-1", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create status %d: %s", resp.StatusCode, b)
	}
	labels := fake.creates[0]["Labels"].(map[string]any)
	if labels[LabelHost] != "web.shop.localhost" {
		t.Fatalf("host label = %v", labels[LabelHost])
	}
	routes, _ := reg.ListRoutes()
	if len(routes) != 1 || routes[0].Host != "web.shop.localhost" || routes[0].Project != "shop" || routes[0].Service != "web" {
		t.Fatalf("routes: %+v", routes)
	}
}

func TestDenyPolicy(t *testing.T) {
	fake := newFakeDocker(t)
	_, _, sock := setupProxy(t, fake, func(_ *registry.Registry, e *policy.Engine) {
		if err := e.SetPolicy(policy.PolicyDeny); err != nil {
			t.Fatal(err)
		}
	})
	resp := createRequest(t, sock, "web-1", nginxCreate())
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var ej map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&ej)
	if !strings.Contains(ej["message"].(string), "prox allow") {
		t.Fatalf("message = %v", ej["message"])
	}
	if len(fake.creates) != 0 {
		t.Fatal("denied create reached Docker")
	}
}

func TestPublishRuleKeepsBinding(t *testing.T) {
	fake := newFakeDocker(t)
	_, _, sock := setupProxy(t, fake, func(_ *registry.Registry, e *policy.Engine) {
		if _, err := e.AddRule(policy.RuleRequest{Name: "web-1", Action: "publish"}); err != nil {
			t.Fatal(err)
		}
	})
	resp := createRequest(t, sock, "web-1", nginxCreate())
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	hc := fake.creates[0]["HostConfig"].(map[string]any)
	if _, ok := hc["PortBindings"]; !ok {
		t.Fatalf("publish must keep PortBindings: %+v", hc)
	}
}

func TestUpgradePassthrough(t *testing.T) {
	fake := newFakeDocker(t)
	_, _, sock := setupProxy(t, fake, nil)
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	req := "POST /v1.45/exec/test/start HTTP/1.1\r\nHost: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Length: 0\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	for i := 0; i < 2; i++ {
		if _, err := conn.Write([]byte(fmt.Sprintf("msg%d\n", i))); err != nil {
			t.Fatal(err)
		}
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line != fmt.Sprintf("echo:msg%d\n", i) {
			t.Fatalf("echo = %q", line)
		}
	}
}

func TestLogsStreaming(t *testing.T) {
	fake := newFakeDocker(t)
	_, _, sock := setupProxy(t, fake, nil)
	resp, err := unixHTTPClient(sock).Get("http://docker/v1.45/containers/web-1/logs?follow=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "first line\nsecond line\n" {
		t.Fatalf("logs = %q", b)
	}
}

func TestNetworkConnectIdempotent(t *testing.T) {
	fake := newFakeDocker(t)
	_, _, sock := setupProxy(t, fake, nil)
	// Fake upstream returns 403 already exists for unknown paths.
	req, _ := http.NewRequest(http.MethodPost, "http://docker/v1.45/networks/gw/connect", strings.NewReader(`{"Container":"deadbeef"}`))
	resp, err := unixHTTPClient(sock).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (already-exists must be idempotent)", resp.StatusCode)
	}
}
