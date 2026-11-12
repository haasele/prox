// Package tcpforward owns configured localhost TCP ports and forwards them to
// the application prox has registered for that port. Before a target exists,
// the port answers with a short prox ownership message and closes.
package tcpforward

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Target maps an owned original port to an upstream address. Empty Target
// means the port is reserved but no application is registered yet.
type Target struct {
	Port   int
	Target string
}

// Change reports a listener lifecycle transition.
type Change struct {
	Port    int
	Target  string
	Started bool
	Updated bool
	Stopped bool
	Err     error
}

type binding struct {
	port   int
	target atomic.Pointer[string]
	ln     net.Listener
}

// Manager owns the configured TCP listeners.
type Manager struct {
	log    *slog.Logger
	mu     sync.Mutex
	active map[int]*binding
	failed map[int]string
}

func New(log *slog.Logger) *Manager {
	return &Manager{log: log, active: map[int]*binding{}, failed: map[int]string{}}
}

// Reconcile starts listeners for new target ports, updates upstream addresses,
// and closes listeners that are no longer configured.
func (m *Manager) Reconcile(desired []Target) []Change {
	want := map[int]Target{}
	for _, t := range desired {
		if t.Port <= 0 {
			continue
		}
		if _, ok := want[t.Port]; ok {
			continue
		}
		want[t.Port] = t
	}

	var changes []Change
	var toClose []*binding
	m.mu.Lock()
	for port, b := range m.active {
		if _, ok := want[port]; !ok {
			delete(m.active, port)
			toClose = append(toClose, b)
			changes = append(changes, Change{Port: port, Target: targetOf(b), Stopped: true})
		}
	}
	for port, t := range want {
		if b, ok := m.active[port]; ok {
			if targetOf(b) != t.Target {
				tc := t.Target
				b.target.Store(&tc)
				changes = append(changes, Change{Port: port, Target: t.Target, Updated: true})
			}
			delete(m.failed, port)
			continue
		}
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			msg := err.Error()
			if m.failed[port] != msg {
				m.failed[port] = msg
				changes = append(changes, Change{Port: port, Target: t.Target, Err: err})
			}
			continue
		}
		b := &binding{port: port, ln: ln}
		tc := t.Target
		b.target.Store(&tc)
		m.active[port] = b
		delete(m.failed, port)
		changes = append(changes, Change{Port: port, Target: t.Target, Started: true})
		go b.serve(m.log)
	}
	m.mu.Unlock()

	for _, b := range toClose {
		_ = b.ln.Close()
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Port < changes[j].Port })
	return changes
}

// Close stops all owned TCP listeners.
func (m *Manager) Close() {
	m.mu.Lock()
	active := m.active
	m.active = map[int]*binding{}
	m.failed = map[int]string{}
	m.mu.Unlock()
	for _, b := range active {
		_ = b.ln.Close()
	}
}

func targetOf(b *binding) string {
	if p := b.target.Load(); p != nil {
		return *p
	}
	return ""
}

func (b *binding) serve(log *slog.Logger) {
	for {
		conn, err := b.ln.Accept()
		if err != nil {
			return
		}
		go b.handle(conn, log)
	}
}

func (b *binding) handle(conn net.Conn, log *slog.Logger) {
	defer conn.Close()
	target := targetOf(b)
	if target == "" {
		_, _ = fmt.Fprintf(conn, "prox: port %d is owned by prox; no application is registered yet\r\n", b.port)
		return
	}
	upstream, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		log.Debug("tcp forward dial", "port", b.port, "target", target, "err", err)
		_, _ = fmt.Fprintf(conn, "prox: upstream %s is unavailable\r\n", target)
		return
	}
	defer upstream.Close()

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, conn)
		closeWrite(upstream)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(conn, upstream)
		closeWrite(conn)
		done <- struct{}{}
	}()
	<-done
	<-done
}

func closeWrite(c net.Conn) {
	if tcp, ok := c.(*net.TCPConn); ok {
		_ = tcp.CloseWrite()
		return
	}
	_ = c.Close()
}
