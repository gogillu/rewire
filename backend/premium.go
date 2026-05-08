// Package main — Premium tier (Intense Mode + leaderboard).
//
// Premium = lifetime bearer-token access. Tokens are minted only after a
// payment_orders row is approved by an admin (see buy.go). Token format:
//
//   <16-byte random base64url>.<HMAC-SHA256(secret, random) base64url>
//
// We store only the SHA-256 hash of the token in `premium_tokens`. The raw
// token is shown to the user once (in /buy completion + the email) and
// then forgotten by us, so a DB leak does not unlock anyone's account.
package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const premiumSchema = `
CREATE TABLE IF NOT EXISTS premium_tokens (
    token_hash    TEXT PRIMARY KEY,         -- sha256(raw token), hex
    email         TEXT NOT NULL,
    order_id      TEXT NOT NULL,
    issued_at     INTEGER NOT NULL,
    revoked_at    INTEGER,
    last_seen_at  INTEGER
);
CREATE INDEX IF NOT EXISTS idx_pt_email ON premium_tokens(email);
CREATE INDEX IF NOT EXISTS idx_pt_order ON premium_tokens(order_id);

-- Per-token preferences (intense mode vibe selection, category prefs).
CREATE TABLE IF NOT EXISTS premium_prefs (
    token_hash    TEXT PRIMARY KEY,
    vibes         TEXT NOT NULL DEFAULT '',  -- comma-separated subset of allowed vibes
    categories    TEXT NOT NULL DEFAULT 'bollywood',
    updated_at    INTEGER NOT NULL
);
`

// loadOrCreateSecret returns the 32-byte HMAC secret used for token signing.
// Persisted to data/premium-secret.bin. Created if missing.
func (s *Server) loadOrCreateSecret() ([]byte, error) {
	p := filepath.Join(s.dataDir, "premium-secret.bin")
	if data, err := os.ReadFile(p); err == nil && len(data) >= 32 {
		return data[:32], nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	if err := os.WriteFile(p, buf, 0o600); err != nil {
		return nil, err
	}
	// Safety net per rubber-duck feedback: if the DB already has tokens but
	// no secret, refuse to silently mint a new key (would invalidate
	// everyone's lifetime access).
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM premium_tokens WHERE revoked_at IS NULL`).Scan(&n)
	if n > 0 {
		return nil, errors.New("premium-secret.bin missing but premium_tokens has live rows; refusing to rotate")
	}
	return buf, nil
}

// mintToken returns a new raw token + its SHA-256 hash.
func (s *Server) mintToken() (raw, hash string, err error) {
	r := make([]byte, 16)
	if _, err = rand.Read(r); err != nil {
		return
	}
	mac := hmac.New(sha256.New, s.premiumSecret)
	mac.Write(r)
	sig := mac.Sum(nil)
	raw = base64.RawURLEncoding.EncodeToString(r) + "." + base64.RawURLEncoding.EncodeToString(sig)
	sum := sha256.Sum256([]byte(raw))
	hash = strings.ToLower(hexEncode(sum[:]))
	return
}

// verifyTokenSignature returns true if the token's HMAC matches our secret.
// This is a cheap pre-check before the DB lookup so we don't even touch the
// DB for obviously malformed tokens.
func (s *Server) verifyTokenSignature(raw string) bool {
	parts := strings.SplitN(raw, ".", 2)
	if len(parts) != 2 {
		return false
	}
	r, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || len(r) != 16 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, s.premiumSecret)
	mac.Write(r)
	want := mac.Sum(nil)
	return subtle.ConstantTimeCompare(sig, want) == 1
}

// premiumTokenFromRequest extracts the bearer token from any of:
//   X-Premium-Token header, ?t= query, premium_token cookie.
// Returns "" if none.
func premiumTokenFromRequest(r *http.Request) string {
	if v := r.Header.Get("X-Premium-Token"); v != "" {
		return strings.TrimSpace(v)
	}
	if v := r.URL.Query().Get("t"); v != "" {
		return v
	}
	if c, err := r.Cookie("premium_token"); err == nil {
		return c.Value
	}
	return ""
}

// requirePremium runs the full check: HMAC verify, DB lookup, not-revoked,
// updates last_seen_at. Returns the matching email on success.
func (s *Server) requirePremium(r *http.Request) (email string, ok bool) {
	raw := premiumTokenFromRequest(r)
	if raw == "" || !s.verifyTokenSignature(raw) {
		return "", false
	}
	sum := sha256.Sum256([]byte(raw))
	hash := strings.ToLower(hexEncode(sum[:]))
	var revoked sql.NullInt64
	err := s.db.QueryRowContext(r.Context(),
		`SELECT email, revoked_at FROM premium_tokens WHERE token_hash = ?`,
		hash).Scan(&email, &revoked)
	if err != nil || revoked.Valid {
		return "", false
	}
	_, _ = s.db.ExecContext(r.Context(),
		`UPDATE premium_tokens SET last_seen_at = ? WHERE token_hash = ?`,
		time.Now().UnixMilli(), hash)
	return email, true
}

// ---------- HTTP handlers ----------

func (s *Server) handlePremiumVerify(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	email, ok := s.requirePremium(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "email": maskEmail(email)})
}

func maskEmail(e string) string {
	at := strings.IndexByte(e, '@')
	if at <= 1 {
		return e
	}
	return string(e[0]) + strings.Repeat("•", at-1) + e[at:]
}

// handlePremiumPrefsGet returns this token's vibe/category preferences.
func (s *Server) handlePremiumPrefsGet(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	_, ok := s.requirePremium(r)
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	hash := tokenHashFromReq(r)
	var vibes, cats string
	_ = s.db.QueryRowContext(r.Context(),
		`SELECT vibes, categories FROM premium_prefs WHERE token_hash = ?`,
		hash).Scan(&vibes, &cats)
	writeJSON(w, http.StatusOK, map[string]any{
		"vibes":      splitCSV(vibes),
		"categories": splitCSV(cats),
	})
}

// handlePremiumPrefsSet updates this token's vibes + categories.
func (s *Server) handlePremiumPrefsSet(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	_, ok := s.requirePremium(r)
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var body struct {
		Vibes      []string `json:"vibes"`
		Categories []string `json:"categories"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	clean := make([]string, 0, len(body.Vibes))
	for _, v := range body.Vibes {
		v = strings.ToLower(strings.TrimSpace(v))
		if validVibe(v) {
			clean = append(clean, v)
		}
	}
	if len(clean) > 6 {
		clean = clean[:6]
	}
	cats := make([]string, 0, len(body.Categories))
	for _, c := range body.Categories {
		c = strings.ToLower(strings.TrimSpace(c))
		switch c {
		case "bollywood", "hollywood", "tv-in", "tv-foreign":
			cats = append(cats, c)
		}
	}
	if len(cats) == 0 {
		cats = []string{"bollywood"}
	}
	hash := tokenHashFromReq(r)
	_, _ = s.db.ExecContext(r.Context(), `
        INSERT INTO premium_prefs (token_hash, vibes, categories, updated_at)
        VALUES (?, ?, ?, ?)
        ON CONFLICT(token_hash) DO UPDATE SET
            vibes=excluded.vibes,
            categories=excluded.categories,
            updated_at=excluded.updated_at
    `, hash, strings.Join(clean, ","), strings.Join(cats, ","), time.Now().UnixMilli())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func tokenHashFromReq(r *http.Request) string {
	raw := premiumTokenFromRequest(r)
	sum := sha256.Sum256([]byte(raw))
	return strings.ToLower(hexEncode(sum[:]))
}

// handlePremiumMovies — token-required. Returns movies with classic +
// vibe-tagged endings filtered by the user's vibe selection (if any).
// v1.5.0: filters by region/kind based on user's `categories` prefs.
//   bollywood   → region=bollywood,  kind=movie
//   hollywood   → region=hollywood,  kind=movie
//   tv-in       → region=bollywood,  kind=tv
//   tv-foreign  → region∈{hollywood,world}, kind=tv
// Defaults to bollywood when prefs are empty (backwards compatible).
func (s *Server) handlePremiumMovies(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if _, ok := s.requirePremium(r); !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	hash := tokenHashFromReq(r)
	var vibesCSV, catsCSV string
	_ = s.db.QueryRowContext(r.Context(),
		`SELECT vibes, COALESCE(categories,'') FROM premium_prefs WHERE token_hash = ?`,
		hash).Scan(&vibesCSV, &catsCSV)
	vibes := splitCSV(vibesCSV)
	cats := splitCSV(catsCSV)
	if len(cats) == 0 {
		cats = []string{"bollywood"}
	}
	movies, err := s.loadMovies(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	movies = filterMoviesByCategories(movies, cats)
	type vibeEnding struct {
		Ending
		Vibe string `json:"vibe,omitempty"`
	}
	type movieOut struct {
		Movie
		VibeEndings []vibeEnding `json:"vibe_endings"`
	}
	out := make([]movieOut, 0, len(movies))
	for _, m := range movies {
		row := movieOut{Movie: m}
		ves, vs, _ := s.loadVibeEndings(r.Context(), m.ID, vibes)
		for i, e := range ves {
			v := ""
			if i < len(vs) {
				v = vs[i]
			}
			row.VibeEndings = append(row.VibeEndings, vibeEnding{Ending: e, Vibe: v})
		}
		out = append(out, row)
	}
	body, _ := json.Marshal(map[string]any{
		"movies":     out,
		"generated":  time.Now().UTC().Format(time.RFC3339),
		"count":      len(out),
		"vibes":      vibes,
		"categories": cats,
	})
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(body)
}

// handlePremiumLike updates a vibe ending's like count.
func (s *Server) handlePremiumLike(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if _, ok := s.requirePremium(r); !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var body struct {
		EndingID int64 `json:"ending_id"`
		Vibe     bool  `json:"vibe"` // true = vibe_endings, false = endings
		Delta    int64 `json:"delta"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if body.EndingID == 0 {
		http.Error(w, "ending_id required", http.StatusBadRequest)
		return
	}
	if body.Delta > 1 {
		body.Delta = 1
	}
	if body.Delta < -1 {
		body.Delta = -1
	}
	if body.Delta == 0 {
		body.Delta = 1
	}
	tbl := "endings"
	if body.Vibe {
		tbl = "vibe_endings"
	}
	_, err := s.db.ExecContext(r.Context(),
		`UPDATE `+tbl+` SET likes = MAX(0, likes + ?) WHERE id = ?`,
		body.Delta, body.EndingID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handlePremiumLeaderboard — token required. Returns top vibe endings
// per (region, kind) bucket so the user sees a balanced leaderboard
// instead of one region dominating. Up to 15 endings per bucket; the
// frontend renders section headers between buckets via the `bucket`
// field.
func (s *Server) handlePremiumLeaderboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if _, ok := s.requirePremium(r); !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `
        SELECT v.id, v.movie_id, v.vibe, v.text, v.likes,
               m.title, m.year, m.poster_url,
               COALESCE(m.region,'bollywood'), COALESCE(m.kind,'movie')
        FROM vibe_endings v
        JOIN movies m ON m.id = v.movie_id
        ORDER BY v.likes DESC, v.id ASC
    `)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	type row struct {
		EndingID  int64  `json:"ending_id"`
		MovieID   string `json:"movie_id"`
		Vibe      string `json:"vibe"`
		Text      string `json:"text"`
		Likes     int64  `json:"likes"`
		Title     string `json:"title"`
		Year      int    `json:"year"`
		PosterURL string `json:"poster_url"`
		Region    string `json:"region"`
		Kind      string `json:"kind"`
		Bucket    string `json:"bucket"` // "bollywood-movie" | "bollywood-tv" | "hollywood-movie" | "hollywood-tv" | "world"
		Label     string `json:"label"`  // human-readable section title
	}
	bucketOrder := []string{"bollywood-movie", "hollywood-movie", "bollywood-tv", "hollywood-tv", "world"}
	bucketLabels := map[string]string{
		"bollywood-movie": "🇮🇳 Bollywood Films",
		"hollywood-movie": "🌍 Hollywood Films",
		"bollywood-tv":    "📺 Indian Series",
		"hollywood-tv":    "📺 International Series",
		"world":           "🎌 World Cinema",
	}
	const perBucket = 15
	buckets := map[string][]row{}
	moviesSeenInBucket := map[string]map[string]bool{}
	for _, b := range bucketOrder {
		moviesSeenInBucket[b] = map[string]bool{}
	}
	for rows.Next() {
		var rr row
		if err := rows.Scan(&rr.EndingID, &rr.MovieID, &rr.Vibe, &rr.Text, &rr.Likes,
			&rr.Title, &rr.Year, &rr.PosterURL, &rr.Region, &rr.Kind); err != nil {
			continue
		}
		region, kind := regionKindAllowed(rr.Region, rr.Kind)
		var b string
		switch {
		case region == "bollywood" && kind == "movie":
			b = "bollywood-movie"
		case region == "hollywood" && kind == "movie":
			b = "hollywood-movie"
		case region == "bollywood" && kind == "tv":
			b = "bollywood-tv"
		case region == "hollywood" && kind == "tv":
			b = "hollywood-tv"
		default:
			b = "world"
		}
		// Cap each bucket at perBucket and ensure each movie shows at
		// most ONE entry per bucket (highest-liked wins) so a single
		// movie can't crowd out the rest.
		if len(buckets[b]) >= perBucket {
			continue
		}
		if moviesSeenInBucket[b][rr.MovieID] {
			continue
		}
		moviesSeenInBucket[b][rr.MovieID] = true
		rr.Region = region
		rr.Kind = kind
		rr.Bucket = b
		rr.Label = bucketLabels[b]
		buckets[b] = append(buckets[b], rr)
	}
	out := make([]row, 0, perBucket*len(bucketOrder))
	sectionLabels := []map[string]any{}
	for _, b := range bucketOrder {
		if len(buckets[b]) == 0 {
			continue
		}
		sectionLabels = append(sectionLabels, map[string]any{
			"bucket": b,
			"label":  bucketLabels[b],
			"count":  len(buckets[b]),
		})
		out = append(out, buckets[b]...)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rows":     out,
		"sections": sectionLabels,
	})
}

// handlePremiumFrontend — serves frontend-premium/. Always accessible (the
// HTML is a token-checking shell that redirects to /buy if no token).
func (s *Server) handlePremiumFrontend() http.Handler {
	dir := filepath.Join(filepath.Dir(s.frontendDir), "frontend-premium")
	if _, err := os.Stat(dir); err != nil {
		dir = s.frontendDir
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/premium")
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
			w.Header().Set("Cache-Control", "no-cache, no-store")
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

// hexEncode is a tiny stdlib-free hex encoder used by token hashing so we
// don't pull encoding/hex into this file alongside encoding/base64.
func hexEncode(b []byte) string {
	const hex = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hex[v>>4]
		out[i*2+1] = hex[v&0x0f]
	}
	return string(out)
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
