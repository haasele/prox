// Package hints answers on the original host port of a proxed HTTP route and
// redirects the browser to the prox hostname. It is intentionally HTTP-only:
// a redirect page is meaningful for web apps, not for plain TCP protocols.
package hints

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Hint describes one original-port listener.
type Hint struct {
	Port int
	Name string
	URL  string
}

// Change reports a listener lifecycle transition.
type Change struct {
	Port    int
	URL     string
	Started bool
	Updated bool
	Stopped bool
	Err     error
}

type binding struct {
	hint atomic.Pointer[Hint]
	ln   net.Listener
	srv  *http.Server
}

// Manager owns the original-port listeners.
type Manager struct {
	log    *slog.Logger
	mu     sync.Mutex
	active map[int]*binding
	failed map[int]string
}

func New(log *slog.Logger) *Manager {
	return &Manager{log: log, active: map[int]*binding{}, failed: map[int]string{}}
}

// Reconcile starts listeners for new hints, updates changed ones, and closes
// listeners whose hint disappeared. It is idempotent.
func (m *Manager) Reconcile(desired []Hint) []Change {
	want := map[int]Hint{}
	for _, h := range desired {
		if h.Port <= 0 {
			continue
		}
		if _, ok := want[h.Port]; ok {
			continue
		}
		want[h.Port] = h
	}

	var changes []Change
	var toClose []*binding
	m.mu.Lock()
	for port, b := range m.active {
		if _, ok := want[port]; !ok {
			delete(m.active, port)
			toClose = append(toClose, b)
			changes = append(changes, Change{Port: port, URL: b.hint.Load().URL, Stopped: true})
		}
	}
	for port, h := range want {
		if b, ok := m.active[port]; ok {
			prev := b.hint.Load()
			if prev.Name != h.Name || prev.URL != h.URL {
				hcopy := h
				b.hint.Store(&hcopy)
				changes = append(changes, Change{Port: port, URL: h.URL, Updated: true})
			}
			delete(m.failed, port)
			continue
		}
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			msg := err.Error()
			if m.failed[port] != msg {
				m.failed[port] = msg
				changes = append(changes, Change{Port: port, URL: h.URL, Err: err})
			}
			continue
		}
		b := &binding{ln: ln}
		hcopy := h
		b.hint.Store(&hcopy)
		b.srv = &http.Server{Handler: b, ReadHeaderTimeout: 10 * time.Second}
		m.active[port] = b
		delete(m.failed, port)
		changes = append(changes, Change{Port: port, URL: h.URL, Started: true})
		go func() {
			if err := b.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
				m.log.Warn("original-port hint listener", "port", port, "err", err)
			}
		}()
	}
	m.mu.Unlock()

	for _, b := range toClose {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = b.srv.Shutdown(ctx)
		cancel()
		_ = b.ln.Close()
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Port < changes[j].Port })
	return changes
}

// Close stops all hint listeners.
func (m *Manager) Close() {
	m.mu.Lock()
	active := m.active
	m.active = map[int]*binding{}
	m.failed = map[int]string{}
	m.mu.Unlock()
	for _, b := range active {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = b.srv.Shutdown(ctx)
		cancel()
		_ = b.ln.Close()
	}
}

func (b *binding) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := b.hint.Load()
	if h == nil {
		http.NotFound(w, r)
		return
	}
	if h.URL == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, fmt.Sprintf("prox owns port %d; no application is registered yet", h.Port), http.StatusServiceUnavailable)
			return
		}
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		fmt.Fprintf(w, ownedPage, h.Port, h.Port)
		return
	}
	target := strings.TrimRight(h.URL, "/")
	if uri := r.URL.RequestURI(); uri != "" && uri != "/" {
		target += uri
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Redirect(w, r, target, http.StatusTemporaryRedirect)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Prox-Route", h.URL)
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	name := h.Name
	if name == "" {
		name = h.URL
	}
	fmt.Fprintf(w, page, html.EscapeString(name), html.EscapeString(target),
		html.EscapeString(name), h.Port, html.EscapeString(h.URL), html.EscapeString(h.URL))
}

const ownedPage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>prox: port %d</title>
<style>
  :root { color-scheme: light dark; }
  body { font: 16px/1.55 system-ui, sans-serif; max-width: 44rem; margin: 10vh auto; padding: 0 1.5rem; }
  code { padding: .1rem .35rem; border-radius: 4px; background: rgba(127,127,127,.18); }
  .muted { opacity: .65; }
</style>
</head>
<body>
<h1>prox owns port %d</h1>
<p>No application is registered on this port yet.</p>
<p>Start a container through the prox Docker socket, or run a dev server with
<code>prox run</code>. It will appear here with a name and a redirect.</p>
<p class="muted">This listener is reserved by prox so the port cannot be taken
by an unrelated process.</p>
</body>
</html>
`

const page = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>prox: %s</title>
<meta http-equiv="refresh" content="2;url=%s">
<style>
  :root { color-scheme: light dark; }
  body { font: 16px/1.55 system-ui, sans-serif; max-width: 44rem; margin: 10vh auto; padding: 0 1.5rem; }
  h1 { margin-bottom: .25rem; }
  code { padding: .1rem .35rem; border-radius: 4px; background: rgba(127,127,127,.18); }
  .muted { opacity: .65; }
</style>
</head>
<body>
<h1>You're trying to access %s</h1>
<p>This is the original address <code>http://localhost:%d</code>.
prox routes this application at <a href="%s"><code>%s</code></a>.</p>
<p>Redirecting to the prox URL…</p>
<p class="muted">This page is served by prox, not by the application.</p>
</body>
</html>
`
