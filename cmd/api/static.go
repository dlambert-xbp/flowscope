package main

import (
	"embed"
	"io/fs"
	"net/http"
)

// staticFS holds the live HTML dashboard served at /. The page polls
// /api/summary every two seconds and renders a small dashboard. It is
// the placeholder until the React SPA in web/ ships.
//
//go:embed static
var staticFS embed.FS

func staticHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// embed never fails at runtime if it compiled; this is a
		// defensive panic that surfaces during local debugging only.
		panic(err)
	}
	return http.FileServer(http.FS(sub))
}
