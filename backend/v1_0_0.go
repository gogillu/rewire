// Package main — frozen v1.0.0 archive.
//
// Serves the snapshot taken at v1.0.0-freeze time. The frozen frontend lives
// in frontend-1.0.0/ and reads movies from a static JSON file dumped at
// freeze time (data/snapshots/v1_0_0-movies.json). Likes on this archive
// are read-only — POST /api/v1.0.0/like is accepted but discarded so that
// the archived experience never mutates.
package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// handleV1FrozenMovies serves the snapshot JSON. The snapshot was generated
// at v1.0.0 freeze time and is shipped along with the binary.
func (s *Server) handleV1FrozenMovies(w http.ResponseWriter, r *http.Request) {
	snap := filepath.Join(s.dataDir, "snapshots", "v1_0_0-movies.json")
	body, err := os.ReadFile(snap)
	if err != nil {
		http.Error(w, "snapshot missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Long cache — frozen content never changes.
	w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	_, _ = w.Write(body)
}

// handleV1FrozenLike accepts POSTs from the archive frontend so the JS
// doesn't error, but the frozen archive's like counts never move.
func (s *Server) handleV1FrozenLike(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "frozen": true})
}

// handleV1FrozenFrontend serves the frontend-1.0.0/ directory under /1.0.0.
func (s *Server) handleV1FrozenFrontend() http.Handler {
	dir := filepath.Join(filepath.Dir(s.frontendDir), "frontend-1.0.0")
	if _, err := os.Stat(dir); err != nil {
		dir = s.frontendDir
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/1.0.0")
		if p == "" || p == "/" {
			p = "/index.html"
		}
		clean := filepath.FromSlash(strings.TrimPrefix(p, "/"))
		if strings.Contains(clean, "..") {
			http.NotFound(w, r)
			return
		}
		full := filepath.Join(dir, clean)
		info, err := os.Stat(full)
		if err != nil || info.IsDir() {
			full = filepath.Join(dir, "index.html")
			info, err = os.Stat(full)
			if err != nil {
				http.NotFound(w, r)
				return
			}
		}
		switch {
		case strings.HasSuffix(p, ".html"), p == "/index.html":
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		case strings.HasSuffix(p, ".js"):
			w.Header().Set("Cache-Control", "public, max-age=300")
			w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		case strings.HasSuffix(p, ".css"):
			w.Header().Set("Cache-Control", "public, max-age=300")
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		case strings.HasSuffix(p, ".webmanifest"):
			w.Header().Set("Content-Type", "application/manifest+json")
		default:
			w.Header().Set("Cache-Control", "public, max-age=86400")
		}
		f, err := os.Open(full)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		http.ServeContent(w, r, info.Name(), info.ModTime(), f)
	})
}
