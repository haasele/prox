package web

import (
	"embed"
	"path"
	"strings"
)

//go:embed index.html app.js style.css
var assets embed.FS

func Index() ([]byte, bool) {
	b, err := assets.ReadFile("index.html")
	return b, err == nil
}

func Asset(name string) ([]byte, string, bool) {
	name = path.Clean("/" + name)
	name = strings.TrimPrefix(name, "/")
	switch name {
	case "", "/":
		name = "index.html"
	}
	b, err := assets.ReadFile(name)
	if err != nil {
		return nil, "", false
	}
	ctype := "application/octet-stream"
	switch path.Ext(name) {
	case ".html":
		ctype = "text/html; charset=utf-8"
	case ".js":
		ctype = "application/javascript; charset=utf-8"
	case ".css":
		ctype = "text/css; charset=utf-8"
	case ".svg":
		ctype = "image/svg+xml"
	}
	return b, ctype, true
}
