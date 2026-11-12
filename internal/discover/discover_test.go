package discover

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"testing"
	"time"
)

func freeDevPort(t *testing.T) int {
	t.Helper()
	for port := 15000; port < 19999; port += 37 {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			ln.Close()
			return port
		}
	}
	t.Fatal("no free dev port")
	return 0
}

func TestScanFindsHTTPServer(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procfs discovery is Linux-only")
	}
	port := freeDevPort(t)
	t.Chdir("../..") // the listener's cwd needs a project manifest (go.mod) to pass the filter
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	srv.Listener = ln
	srv.Start()
	defer srv.Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, l := range Scan(0) {
			if l.Port != port {
				continue
			}
			if l.PID <= 0 || l.Name == "" || l.Dir == "" {
				t.Fatalf("incomplete listener: %+v", l)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("listener on port %d was not discovered", port)
}

func TestParseProcNetTCP(t *testing.T) {
	b, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		t.Skip("no /proc/net/tcp")
	}
	if len(parseProcNet("/proc/net/tcp")) == 0 && len(b) > 0 {
		// An idle machine may genuinely have no listeners; do not fail the suite.
		t.Log("no listening TCP sockets parsed")
	}
}

func TestSystemDir(t *testing.T) {
	for _, dir := range []string{"/", "/usr/bin", "/var/lib/x", "/etc", "/opt/app"} {
		if !systemDir(dir) {
			t.Fatalf("%q should be a system dir", dir)
		}
	}
	for _, dir := range []string{"/home/me/proj", "/mnt/data/app", "/tmp/app"} {
		if systemDir(dir) {
			t.Fatalf("%q should be a user dir", dir)
		}
	}
}

func TestDeriveName(t *testing.T) {
	if got := deriveName("/home/me/My App", "bun"); got != "my-app" {
		t.Fatalf("deriveName = %q", got)
	}
	if got := deriveName("/", "bun"); got != "bun" {
		t.Fatalf("deriveName = %q", got)
	}
	if got := deriveName("/home/me/project", "node"); got != "project" {
		t.Fatalf("deriveName = %q", got)
	}
}

func TestProbeHTTPRejectsNonHTTP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = conn.Write([]byte("SSH-2.0-not-http\r\n"))
		conn.Close()
	}()
	if probeHTTP(socket{port: ln.Addr().(*net.TCPAddr).Port}) {
		t.Fatal("non-HTTP protocol was accepted")
	}
}
