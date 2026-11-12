package hints

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
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
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func testManager() *Manager {
	return New(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestHintPageAndRedirect(t *testing.T) {
	m := testManager()
	defer m.Close()
	port := freePort(t)
	url := "https://myapp.localhost:8443"
	changes := m.Reconcile([]Hint{{Port: port, Name: "myapp", URL: url}})
	if len(changes) != 1 || !changes[0].Started || changes[0].Err != nil {
		t.Fatalf("changes = %+v", changes)
	}

	client := &http.Client{Timeout: 2 * time.Second}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	resp, err := client.Get(base + "/deep?x=1")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	for _, want := range []string{"myapp", url + "/deep?x=1", fmt.Sprintf("http://localhost:%d", port), "prox routes this application"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("body missing %q: %s", want, body)
		}
	}
	if got := resp.Header.Get("X-Prox-Route"); got != url {
		t.Fatalf("X-Prox-Route = %q", got)
	}

	req, _ := http.NewRequest(http.MethodPost, base+"/api", nil)
	noRedirect := &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	post, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	post.Body.Close()
	if post.StatusCode != http.StatusTemporaryRedirect || post.Header.Get("Location") != url+"/api" {
		t.Fatalf("post status=%d location=%q", post.StatusCode, post.Header.Get("Location"))
	}

	// Updating the hint changes the page and X-Prox-Route.
	newURL := "https://renamed.localhost:8443"
	if changes := m.Reconcile([]Hint{{Port: port, Name: "renamed", URL: newURL}}); len(changes) != 1 || !changes[0].Updated {
		t.Fatalf("update changes = %+v", changes)
	}
	resp, err = client.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "renamed") || !strings.Contains(string(body), newURL) {
		t.Fatalf("updated body = %s", body)
	}

	// Removing the hint closes the listener.
	changes = m.Reconcile(nil)
	if len(changes) != 1 || !changes[0].Stopped {
		t.Fatalf("stop changes = %+v", changes)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if err != nil {
			break
		}
		conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("hint listener still accepting connections")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestHintPlaceholderBecomesApp(t *testing.T) {
	m := testManager()
	defer m.Close()
	port := freePort(t)
	changes := m.Reconcile([]Hint{{Port: port, Name: "prox"}})
	if len(changes) != 1 || !changes[0].Started || changes[0].URL != "" {
		t.Fatalf("changes = %+v", changes)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	base := fmt.Sprintf("http://127.0.0.1:%d/", port)
	resp, err := client.Get(base)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "prox owns port") {
		t.Fatalf("placeholder status=%d body=%s", resp.StatusCode, body)
	}

	url := "https://app.localhost:8443"
	changes = m.Reconcile([]Hint{{Port: port, Name: "app", URL: url}})
	if len(changes) != 1 || !changes[0].Updated || changes[0].URL != url {
		t.Fatalf("update changes = %+v", changes)
	}
	resp, err = client.Get(base)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "app") || !strings.Contains(string(body), url) {
		t.Fatalf("app body = %s", body)
	}
}

func TestHintBusyPortReportedOnce(t *testing.T) {
	m := testManager()
	defer m.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	h := []Hint{{Port: port, Name: "app", URL: "https://app.localhost"}}
	changes := m.Reconcile(h)
	if len(changes) != 1 || changes[0].Err == nil || changes[0].Started {
		t.Fatalf("changes = %+v", changes)
	}
	if changes := m.Reconcile(h); len(changes) != 0 {
		t.Fatalf("repeated failure should be silent: %+v", changes)
	}
}
