// Package web holds the daemon's embedded management UI: a single
// self-contained HTML page (inline CSS and vanilla JavaScript, no build
// step, no external network requests) served at "/", sharing the REST API
// origin per design.md. It talks to the existing REST API only and never
// receives secrets, since the API itself already redacts them.
package web

import (
	_ "embed"
	"net/http"
)

//go:embed index.html
var indexHTML []byte

// Handler serves the embedded management UI page.
func Handler() http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = response.Write(indexHTML)
	})
}
