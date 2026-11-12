// Command demo is a tiny app for the live prox demo. It only knows it is an
// HTTP server on port 3000; prox is what gives it a public HTTPS name.
package main

import (
	"fmt"
	"html"
	"net/http"
	"os"
	"time"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}
	originalURL := os.Getenv("ORIGINAL_URL")
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		proto := r.Header.Get("X-Forwarded-Proto")
		if proto == "" {
			proto = "http"
		}
		currentURL := proto + "://" + r.Host + r.URL.RequestURI()
		fmt.Fprintf(w, `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>prox live demo</title>
<style>
  :root { color-scheme: light dark; }
  body { font: 16px/1.55 system-ui, sans-serif; max-width: 46rem; margin: 10vh auto; padding: 0 1.5rem; }
  h1 { margin-bottom: .25rem; }
  .tag { display: inline-block; padding: .15rem .55rem; border-radius: 999px;
         background: #2b8a3e; color: #fff; font-size: .8rem; vertical-align: middle; }
  code { padding: .1rem .35rem; border-radius: 4px; background: rgba(127,127,127,.18); }
  table { border-collapse: collapse; margin: 1rem 0; }
  td { padding: .2rem 1rem .2rem 0; vertical-align: top; }
  .muted { opacity: .65; }
  .url { font-size: 1.15rem; font-weight: 600; }
</style>
</head>
<body>
<h1>prox live demo <span class="tag">routed</span></h1>
<p>You are reading this at:</p>
<p class="url"><a href="%s"><code>%s</code></a></p>
<p>That is the public prox URL. The app itself only listens on container
port <code>%s</code> with no host port.</p>
<table>
  <tr><td>Public URL</td><td><code>%s</code></td></tr>
  <tr><td>Original URL</td><td><code>%s</code> — answered by prox with a redirect page</td></tr>
  <tr><td>Host header</td><td><code>%s</code></td></tr>
  <tr><td>Container port</td><td><code>%s</code></td></tr>
  <tr><td>Request path</td><td><code>%s</code></td></tr>
  <tr><td>X-Forwarded-Host</td><td><code>%s</code></td></tr>
  <tr><td>X-Forwarded-Proto</td><td><code>%s</code></td></tr>
  <tr><td>Time</td><td><code>%s</code></td></tr>
</table>
<p class="muted">Docker printed the mapping at create time. The host port from
<code>docker run</code> is answered by prox with a hint page that redirects
here; this page is served by the prox gateway, not by the host port.</p>
</body>
</html>
`, html.EscapeString(currentURL), html.EscapeString(currentURL), html.EscapeString(port),
			html.EscapeString(currentURL),
			html.EscapeString(originalURL),
			html.EscapeString(r.Host), html.EscapeString(port),
			html.EscapeString(r.URL.Path),
			html.EscapeString(r.Header.Get("X-Forwarded-Host")),
			html.EscapeString(r.Header.Get("X-Forwarded-Proto")),
			html.EscapeString(time.Now().Format(time.RFC3339)))
	})
	fmt.Printf("demo app listening on :%s\n", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
