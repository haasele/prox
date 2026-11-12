package router

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func waitForSync() { time.Sleep(100 * time.Millisecond) }

func TestCaddyHTTPAndTLS(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello from " + r.Host))
	}))
	defer upstream.Close()
	upURL := strings.TrimPrefix(upstream.URL, "http://")

	httpPort, httpsPort, loopPort := freePort(t), freePort(t), freePort(t)
	caDir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := New(caDir, log)
	t.Cleanup(func() { _ = r.Stop() })

	in := Input{
		HTTPPort: httpPort, HTTPSPort: httpsPort, TLSLoopbackPort: loopPort,
		Routes: []HTTPRoute{
			{Host: "app.localhost", Upstream: upURL, Status: "up", Redirect: true},
			{Host: "plain.localhost", Upstream: upURL, Status: "up", Redirect: false},
		},
		DashboardHost: "prox.localhost", DashboardUpstream: upURL,
		CAStorage: caDir,
	}
	if err := r.Sync(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	caPEM, err := r.RootCert(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("bad CA PEM")
	}
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "app.localhost"},
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", httpsPort))
			},
		},
	}

	var body string
	deadline := time.Now().Add(8 * time.Second)
	for {
		resp, err := client.Get("https://app.localhost/")
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			body = string(b)
			if resp.StatusCode != 200 {
				t.Fatalf("status = %d body %q", resp.StatusCode, body)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("HTTPS request never succeeded: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(body, "hello from app.localhost") {
		t.Fatalf("body = %q", body)
	}

	// HTTP redirect on the plain listener.
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/some/path?q=1", httpPort), nil)
	req.Host = "app.localhost"
	tr := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, fmt.Sprintf("127.0.0.1:%d", httpPort))
	}}
	hc := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusPermanentRedirect {
		t.Fatalf("redirect status = %d", resp.StatusCode)
	}
	want := fmt.Sprintf("https://app.localhost:%d/some/path?q=1", httpsPort)
	if got := resp.Header.Get("Location"); got != want {
		t.Fatalf("Location = %q, want %q", got, want)
	}

	// Unknown HTTP hosts get a clear 503 instead of a redirect to a TLS error.
	req, _ = http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/", httpPort), nil)
	req.Host = "unknown.localhost"
	resp, err = hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unknown host status = %d", resp.StatusCode)
	}

	// Rules with redirect disabled are served over plain HTTP.
	req, _ = http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/", httpPort), nil)
	req.Host = "plain.localhost"
	resp, err = hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "hello from plain.localhost") {
		t.Fatalf("plain route status=%d body=%q", resp.StatusCode, b)
	}

	// Dashboard host serves through the same proxy.
	client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: pool, ServerName: "prox.localhost"}
	resp, err = client.Get("https://prox.localhost/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("dashboard status = %d", resp.StatusCode)
	}

	// A down route returns the friendly 503 page.
	in.Routes[0].Status = "down"
	in.Routes[0].Upstream = ""
	if err := r.Sync(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	waitForSync()
	client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: pool, ServerName: "app.localhost"}
	resp, err = client.Get("https://app.localhost/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 503 || !strings.Contains(string(b), "503") || !strings.Contains(string(b), "dashboard") {
		t.Fatalf("down page status=%d body=%q", resp.StatusCode, b)
	}
}

func TestCaddyTCPGatewayAndSNI(t *testing.T) {
	// Echo TCP server.
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoLn.Close()
	echoAddr := echoLn.Addr().String()
	gotBytes := make(chan []byte, 8)
	go func() {
		for {
			c, err := echoLn.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				n, _ := c.Read(buf)
				if n > 0 {
					gotBytes <- append([]byte(nil), buf[:n]...)
				}
			}(c)
		}
	}()

	httpPort, httpsPort, loopPort := freePort(t), freePort(t), freePort(t)
	gwPort := freePort(t)
	caDir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := New(caDir, log)
	t.Cleanup(func() { _ = r.Stop() })
	in := Input{
		HTTPPort: httpPort, HTTPSPort: httpsPort, TLSLoopbackPort: loopPort,
		DashboardHost: "prox.localhost", CAStorage: caDir,
		TCP: []TCPRoute{{Port: gwPort, Upstream: echoAddr, Status: "up"}},
		SNI: []SNIRoute{{Host: "db.localhost", Upstream: echoAddr, Status: "up"}},
	}
	if err := r.Sync(context.Background(), in); err != nil {
		t.Fatal(err)
	}

	// Dedicated gateway port.
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", gwPort), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = conn.Write([]byte("ping"))
	buf := make([]byte, 16)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _ = conn.Read(buf)
	conn.Close()
	select {
	case b := <-gotBytes:
		if string(b) != "ping" {
			t.Fatalf("echo got %q", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("echo server did not receive gateway traffic")
	}

	// SNI front: a TLS client for db.localhost must be passed through to the
	// echo upstream (handshake fails because echo is not TLS), not to the
	// internal HTTPS server (which would complete the handshake).
	raw, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", httpsPort), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	tconn := tls.Client(raw, &tls.Config{ServerName: "db.localhost", InsecureSkipVerify: true})
	_ = tconn.SetDeadline(time.Now().Add(2 * time.Second))
	if err := tconn.Handshake(); err == nil {
		t.Fatal("expected handshake to fail against the echo upstream")
	}
	select {
	case b := <-gotBytes:
		if len(b) < 3 || b[0] != 0x16 {
			t.Fatalf("SNI traffic did not reach echo: %q", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SNI route did not reach the echo upstream")
	}
}
