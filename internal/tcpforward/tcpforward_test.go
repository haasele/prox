package tcpforward

import (
	"fmt"
	"io"
	"log/slog"
	"net"
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

func echoServer(t *testing.T) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

func testManager() *Manager {
	return New(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestForwardPlaceholderAndTarget(t *testing.T) {
	m := testManager()
	defer m.Close()
	port := freePort(t)
	changes := m.Reconcile([]Target{{Port: port}})
	if len(changes) != 1 || !changes[0].Started || changes[0].Err != nil {
		t.Fatalf("changes = %+v", changes)
	}

	// No target: the port is reserved and explains that.
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	b, _ := io.ReadAll(conn)
	conn.Close()
	if !strings.HasPrefix(string(b), "prox") {
		t.Fatalf("placeholder response = %q", b)
	}

	echo, closeEcho := echoServer(t)
	defer closeEcho()
	changes = m.Reconcile([]Target{{Port: port, Target: echo}})
	if len(changes) != 1 || !changes[0].Updated {
		t.Fatalf("update changes = %+v", changes)
	}
	conn, err = net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if string(buf) != "hello" {
		t.Fatalf("echo = %q", buf)
	}

	changes = m.Reconcile(nil)
	if len(changes) != 1 || !changes[0].Stopped {
		t.Fatalf("stop changes = %+v", changes)
	}
	if conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("forwarder still listening")
	}
}

func TestForwardBusyPortReportedOnce(t *testing.T) {
	m := testManager()
	defer m.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	targets := []Target{{Port: port, Target: "127.0.0.1:1"}}
	changes := m.Reconcile(targets)
	if len(changes) != 1 || changes[0].Err == nil {
		t.Fatalf("changes = %+v", changes)
	}
	if changes := m.Reconcile(targets); len(changes) != 0 {
		t.Fatalf("repeated failure should be silent: %+v", changes)
	}
}
