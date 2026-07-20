// Package agoboardui serves the local board interface from the same origin as
// the API.
//
// The alternative — hosting the interface remotely and having it call
// http://127.0.0.1 — cannot work: a browser blocks an HTTPS page from fetching
// a plaintext localhost origin, and no CORS policy changes that. Widening CORS
// on the local server to accommodate a remote origin would also mean any site
// the user visits could reach their local scheduler. Serving both from one
// loopback origin avoids the question entirely and needs no CORS at all.
//
// The assets are embedded in the binary, so the demo has no build step, no
// external fetch, and nothing to install.
package agoboardui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed assets
var assets embed.FS

// Handler serves the interface. It is mounted under the same mux as the API,
// so the page and its data share an origin.
func Handler() (http.Handler, error) {
	content, err := fs.Sub(assets, "assets")
	if err != nil {
		return nil, err
	}
	files := http.FileServerFS(content)
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// Everything is served from this origin, so the page needs no external
		// script, style, font, or connection. Saying so makes an injected
		// script useless even if one ever got in.
		writer.Header().Set("Content-Security-Policy",
			"default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; form-action 'none'; base-uri 'none'; frame-ancestors 'none'")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		// The board route is a single page; anything that is not a known asset
		// renders it rather than 404ing, so a refresh on a deep link works.
		if request.URL.Path != "/" && !strings.Contains(request.URL.Path, ".") {
			request = request.Clone(request.Context())
			request.URL.Path = "/"
		}
		files.ServeHTTP(writer, request)
	}), nil
}
