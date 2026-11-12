package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	_ "github.com/caddyserver/caddy/v2/modules/standard"
	_ "github.com/mholt/caddy-l4"
)

type HTTPRoute struct {
	Host     string
	Upstream string
	Status   string
	Redirect bool
}

type SNIRoute struct {
	Host     string
	Upstream string
	Status   string
}

type TCPRoute struct {
	Port     int
	Upstream string
	Status   string
}

type Input struct {
	HTTPPort          int
	HTTPSPort         int
	TLSLoopbackPort   int
	HTTPSListen       string
	Routes            []HTTPRoute
	SNI               []SNIRoute
	TCP               []TCPRoute
	DashboardHost     string
	DashboardUpstream string
	DashboardURL      string
	CAStorage         string
}

type Caddy struct {
	CAStorage string
	Log       *slog.Logger

	mu   sync.Mutex
	last []byte
}

func New(caStorage string, log *slog.Logger) *Caddy {
	return &Caddy{CAStorage: caStorage, Log: log}
}

func (c *Caddy) Sync(ctx context.Context, in Input) error {
	in.CAStorage = c.CAStorage
	b, err := BuildConfig(in)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if bytes.Equal(c.last, b) {
		return nil
	}
	if err := caddy.Load(b, true); err != nil {
		return fmt.Errorf("caddy config rejected: %w", err)
	}
	c.last = b
	return nil
}

func (c *Caddy) Stop() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return caddy.Stop()
}

// RootCert returns the PEM of the local root CA, waiting briefly for Caddy to
// generate it on first start.
func (c *Caddy) RootCert(ctx context.Context) ([]byte, error) {
	path := filepath.Join(c.CAStorage, "pki", "authorities", "local", "root.crt")
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err := os.ReadFile(path)
		if err == nil && len(b) > 0 {
			return b, nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return nil, fmt.Errorf("local CA not found at %s: %w", path, err)
			}
			return nil, fmt.Errorf("local CA at %s is empty", path)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

const fallbackHTML = `<!doctype html><html><head><meta charset="utf-8"><title>prox — no route</title>
<style>body{font:16px/1.5 system-ui,sans-serif;background:#0f1115;color:#e6e8eb;display:grid;place-items:center;min-height:100vh;margin:0}
main{max-width:34rem;padding:2rem;border:1px solid #2a2e37;border-radius:12px;background:#161920}
h1{margin:0 0 .5rem;font-size:1.35rem}code{background:#0b0d11;padding:.15rem .35rem;border-radius:4px}
a{color:#7cc4ff}.muted{color:#9aa3af}</style></head>
<body><main><h1>503 — service not available</h1>
<p><code>{http.request.host}</code> has no healthy container behind it right now.</p>
<p class="muted">Start the container, or open the <a href="%s">prox dashboard</a> for routes, rules and events.</p>
</main></body></html>`

func BuildConfig(in Input) ([]byte, error) {
	if in.HTTPPort == 0 {
		in.HTTPPort = 80
	}
	if in.HTTPSPort == 0 {
		in.HTTPSPort = 443
	}
	if in.TLSLoopbackPort == 0 {
		in.TLSLoopbackPort = in.HTTPSPort + 1
	}
	if in.DashboardHost == "" {
		in.DashboardHost = "prox.localhost"
	}
	dashURL := in.DashboardURL
	if dashURL == "" {
		dashURL = "https://" + in.DashboardHost
		if in.HTTPSPort != 443 {
			dashURL += fmt.Sprintf(":%d", in.HTTPSPort)
		}
		dashURL += "/"
	}
	page := fmt.Sprintf(fallbackHTML, dashURL)

	dedupe := map[string]HTTPRoute{}
	for _, r := range in.Routes {
		if r.Host == "" {
			continue
		}
		if prev, ok := dedupe[r.Host]; !ok || (prev.Status != "up" && r.Status == "up") {
			dedupe[r.Host] = r
		}
	}
	httpRoutes := make([]HTTPRoute, 0, len(dedupe))
	for _, r := range dedupe {
		httpRoutes = append(httpRoutes, r)
	}
	sort.Slice(httpRoutes, func(i, j int) bool { return httpRoutes[i].Host < httpRoutes[j].Host })

	httpsRoutes := []any{}
	if in.DashboardUpstream != "" {
		httpsRoutes = append(httpsRoutes, hostRoute(in.DashboardHost, reverseProxy(in.DashboardUpstream)))
	}
	subjects := []string{in.DashboardHost}
	for _, r := range httpRoutes {
		subjects = append(subjects, r.Host)
		if r.Status == "up" && r.Upstream != "" {
			httpsRoutes = append(httpsRoutes, hostRoute(r.Host, reverseProxy(r.Upstream)))
		} else {
			httpsRoutes = append(httpsRoutes, hostRoute(r.Host, staticPage(503, page)))
		}
	}
	httpsRoutes = append(httpsRoutes, map[string]any{"handle": []any{staticPage(503, page)}})
	httpsServer := map[string]any{
		"listen":                  []string{fmt.Sprintf("127.0.0.1:%d", in.TLSLoopbackPort)},
		"routes":                  httpsRoutes,
		"tls_connection_policies": []any{map[string]any{}},
		"strict_sni_host":         true,
		"automatic_https":         map[string]any{"disable_redirects": true},
	}

	// Plain HTTP: per-host redirects, direct proxying when a rule disables the
	// redirect, and a 503 for hosts that prox does not know.
	plainRoutes := []any{}
	hostSuffix := ""
	if in.HTTPSPort != 443 {
		hostSuffix = fmt.Sprintf(":%d", in.HTTPSPort)
	}
	for _, r := range httpRoutes {
		switch {
		case r.Status == "up" && r.Upstream != "" && !r.Redirect:
			plainRoutes = append(plainRoutes, hostRoute(r.Host, reverseProxy(r.Upstream)))
		case r.Status == "up" && r.Upstream != "":
			plainRoutes = append(plainRoutes, hostRoute(r.Host, redirectHandler(hostSuffix)))
		default:
			plainRoutes = append(plainRoutes, hostRoute(r.Host, staticPage(503, page)))
		}
	}
	plainRoutes = append(plainRoutes, map[string]any{"handle": []any{staticPage(503, page)}})
	httpServer := map[string]any{
		"listen":          []string{fmt.Sprintf(":%d", in.HTTPPort)},
		"routes":          plainRoutes,
		"automatic_https": map[string]any{"disable": true},
	}

	// Layer4: one front listener on the HTTPS port does SNI routing, and a
	// default passthrough to the in-process TLS server.
	frontRoutes := []any{}
	for _, s := range in.SNI {
		if s.Status != "up" || s.Upstream == "" {
			continue
		}
		subjects = append(subjects, s.Host)
		frontRoutes = append(frontRoutes, map[string]any{
			"match":  []any{map[string]any{"tls": map[string]any{"sni": []string{s.Host}}}},
			"handle": []any{proxyHandler(s.Upstream)},
		})
	}
	frontRoutes = append(frontRoutes, map[string]any{
		"handle": []any{proxyHandler(fmt.Sprintf("127.0.0.1:%d", in.TLSLoopbackPort))},
	})
	listenHost := in.HTTPSListen
	frontListen := fmt.Sprintf("%s:%d", listenHost, in.HTTPSPort)
	l4Servers := map[string]any{
		"front": map[string]any{
			"listen":           []string{frontListen},
			"matching_timeout": int64(5 * time.Second),
			"routes":           frontRoutes,
		},
	}
	for _, t := range in.TCP {
		if t.Port <= 0 || t.Upstream == "" {
			continue
		}
		if t.Port == in.HTTPPort || t.Port == in.HTTPSPort || t.Port == in.TLSLoopbackPort {
			return nil, fmt.Errorf("gateway port %d conflicts with the proxy's own listener", t.Port)
		}
		l4Servers[fmt.Sprintf("tcp-%d", t.Port)] = map[string]any{
			"listen": []string{fmt.Sprintf(":%d", t.Port)},
			"routes": []any{map[string]any{"handle": []any{proxyHandler(t.Upstream)}}},
		}
	}

	subjects = uniqueStrings(subjects)
	tlsPolicy := map[string]any{
		"subjects": subjects,
		"issuers":  []any{map[string]any{"module": "internal", "ca": "local"}},
		"storage":  map[string]any{"module": "file_system", "root": filepath.Join(in.CAStorage, "data")},
	}
	cfg := map[string]any{
		"admin": map[string]any{"disabled": true},
		"logging": map[string]any{
			"logs": map[string]any{
				"default": map[string]any{"level": "ERROR"},
			},
		},
		"apps": map[string]any{
			"pki": map[string]any{
				"certificate_authorities": map[string]any{
					"local": map[string]any{
						"name":             "local",
						"root_common_name": "prox local CA",
						"install_trust":    false,
						"storage":          map[string]any{"module": "file_system", "root": in.CAStorage},
					},
				},
			},
			"tls": map[string]any{
				"automation": map[string]any{
					"policies": []any{tlsPolicy},
				},
			},
			"http": map[string]any{
				"http_port":  in.HTTPPort,
				"https_port": in.HTTPSPort,
				"servers": map[string]any{
					"prox_https": httpsServer,
					"prox_http":  httpServer,
				},
			},
			"layer4": map[string]any{
				"servers": l4Servers,
			},
		},
	}
	return json.Marshal(cfg)
}

func hostRoute(host string, handler map[string]any) map[string]any {
	return map[string]any{
		"match":  []any{map[string]any{"host": []string{host}}},
		"handle": []any{handler},
	}
}

func reverseProxy(upstream string) map[string]any {
	return map[string]any{
		"handler":        "reverse_proxy",
		"upstreams":      []any{map[string]any{"dial": upstream}},
		"flush_interval": -1,
	}
}

func redirectHandler(hostSuffix string) map[string]any {
	return map[string]any{
		"handler":     "static_response",
		"status_code": 308,
		"headers": map[string]any{
			"Location": []string{"https://{http.request.host}" + hostSuffix + "{http.request.uri}"},
		},
	}
}

func proxyHandler(upstream string) map[string]any {
	return map[string]any{
		"handler":   "proxy",
		"upstreams": []any{map[string]any{"dial": []string{upstream}}},
	}
}

func staticPage(status int, body string) map[string]any {
	return map[string]any{
		"handler":     "static_response",
		"status_code": status,
		"headers":     map[string]any{"Content-Type": []string{"text/html; charset=utf-8"}},
		"body":        body,
	}
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
