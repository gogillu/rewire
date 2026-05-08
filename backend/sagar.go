// Package main — /sagar variant.
//
// Sagar's beta-testing feedback (WhatsApp, 7-May 01:42–02:05 IST) zeroed in
// on observability and stat transparency rather than content. Recurring asks:
//
//   - "What does the leaderboard mean?" / "Lead kis basis pe decide ho rha hai"
//   - "Kuch total likes ka bhi hona chahiye na"
//   - "How many people interacted with that film"
//   - "Views bhi hone chahiye total" / "Conversion tabhi pata lagega"
//   - "Db aur event likes mein kya antar hai" → only DB likes matter
//   - "Wo gaana kahan se le rha hai" → song-source visibility
//   - "Some movies didn't have correct music" → flag wrong audio
//
// So /sagar is the "stats-nerd" build:
//   - Same scroll experience, but every card overlays live
//     👁 views · ❤️ likes · ⚡ conversion% next to the title.
//   - 🎵❌ flag-audio button reports wrong-music for a movie.
//   - 📊 leaderboard panel ranks all movies by likes / views / conversion,
//     using DB likes as source of truth.
//   - Every telemetry / feedback POST tags mode='sagar' so dashboards can
//     split metrics from /, /abhinav, /sagar.
//
// No DB schema changes — we derive views from telemetry_events
// (event_type='card_enter') and likes from endings.likes.
package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ---------- /api/sagar/movies ----------
//
// Same shape as /api/movies but each movie carries:
//   - views           : COUNT(DISTINCT anon_id) of card_enter events
//   - likes_total     : SUM(endings.likes) for that movie
//   - conversion_pct  : 100 * likes_total / max(views, 1), rounded to 1 dp
//   - song_query      : whatever populated data/audio/<id>.mp3 (best effort)
//
// We keep the audio_version field so the cache-bust logic added for the
// Piku fix still applies.

type sagarMovie struct {
	ID            string  `json:"id"`
	Title         string  `json:"title"`
	Year          int     `json:"year"`
	IMDBRating    float64 `json:"imdb_rating"`
	Genre         string  `json:"genre"`
	Synopsis      string  `json:"synopsis"`
	ActualEnding  string  `json:"actual_ending"`
	PosterURL     string  `json:"poster_url"`
	BackdropURL   string  `json:"backdrop_url,omitempty"`
	HasAudio      bool    `json:"has_audio"`
	AudioVersion  int64   `json:"audio_version,omitempty"`
	SongQuery     string  `json:"song_query,omitempty"`
	Endings       []Ending `json:"endings"`
	Views         int64   `json:"views"`
	LikesTotal    int64   `json:"likes_total"`
	ConversionPct float64 `json:"conversion_pct"`
}

func (s *Server) handleSagarMovies(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Pull the same movie+endings shape from main.go's helper. We reuse the
	// cached blob if present so the hot path stays cheap. v1.5.1 — pin to
	// bollywood-movie; /sagar pre-dates the global catalog.
	mrows, err := s.db.QueryContext(ctx, `
        SELECT id, title, year, imdb_rating, genre, synopsis,
               actual_ending, poster_url, backdrop_url
        FROM movies
        WHERE COALESCE(region,'bollywood') = 'bollywood'
          AND COALESCE(kind,'movie')       = 'movie'
        ORDER BY sort_order ASC, imdb_rating DESC, year DESC
    `)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	out := make([]sagarMovie, 0, 100)
	idx := map[string]int{}
	for mrows.Next() {
		var m sagarMovie
		if err := mrows.Scan(&m.ID, &m.Title, &m.Year, &m.IMDBRating, &m.Genre,
			&m.Synopsis, &m.ActualEnding, &m.PosterURL, &m.BackdropURL); err != nil {
			mrows.Close()
			http.Error(w, err.Error(), 500)
			return
		}
		m.Endings = []Ending{}
		if info, err := os.Stat(filepath.Join(s.dataDir, "audio", m.ID+".mp3")); err == nil && info.Size() > 50_000 {
			m.HasAudio = true
			m.AudioVersion = info.ModTime().Unix()
		}
		if q, ok := s.songQueryFor(m.ID); ok {
			m.SongQuery = q
		}
		idx[m.ID] = len(out)
		out = append(out, m)
	}
	mrows.Close()

	// Endings + likes (DB likes — Sagar's source of truth).
	erows, err := s.db.QueryContext(ctx, `
        SELECT id, movie_id, model, text, likes
        FROM endings ORDER BY movie_id, slot
    `)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	for erows.Next() {
		var e Ending
		var movieID string
		if err := erows.Scan(&e.ID, &movieID, &e.Model, &e.Text, &e.Likes); err != nil {
			erows.Close()
			http.Error(w, err.Error(), 500)
			return
		}
		if i, ok := idx[movieID]; ok {
			out[i].Endings = append(out[i].Endings, e)
			out[i].LikesTotal += e.Likes
		}
	}
	erows.Close()

	// Per-movie unique-viewer count from telemetry. One scan, populates all.
	vrows, err := s.db.QueryContext(ctx, `
        SELECT movie_id, COUNT(DISTINCT anon_id) AS views
        FROM telemetry_events
        WHERE event_type = 'card_enter' AND movie_id IS NOT NULL AND movie_id != ''
        GROUP BY movie_id
    `)
	if err == nil {
		for vrows.Next() {
			var mid string
			var v int64
			if err := vrows.Scan(&mid, &v); err == nil {
				if i, ok := idx[mid]; ok {
					out[i].Views = v
				}
			}
		}
		vrows.Close()
	}

	// Conversion = likes / views, clamped to [0, 100] so the leaderboard
	// stays sane when historical likes outnumber post-launch views.
	for i := range out {
		if out[i].Views > 0 {
			pct := float64(out[i].LikesTotal) / float64(out[i].Views) * 100.0
			if pct > 100.0 {
				pct = 100.0
			}
			out[i].ConversionPct = float64(int(pct*10+0.5)) / 10.0
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"app_version": "1.0.0+sagar",
		"generated":   time.Now().UTC().Format(time.RFC3339),
		"count":       len(out),
		"movies":      out,
	})
}

// ---------- /api/sagar/leaderboard ----------
//
// Sorted ranking of every movie that has at least one view OR one like.
// Sortable by likes (default), views, or conversion. Sagar can finally see
// "lead kis basis pe decide ho rha hai" and "uske alawa bhi hain (Raazi
// also)" — every movie shows up, not just the top 5.

type sagarLeader struct {
	Rank          int     `json:"rank"`
	ID            string  `json:"id"`
	Title         string  `json:"title"`
	Year          int     `json:"year"`
	PosterURL     string  `json:"poster_url"`
	Views         int64   `json:"views"`
	LikesTotal    int64   `json:"likes_total"`
	ConversionPct float64 `json:"conversion_pct"`
}

func (s *Server) handleSagarLeaderboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sortBy := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sort")))
	if sortBy != "views" && sortBy != "conversion" {
		sortBy = "likes"
	}

	// Build a unified per-movie aggregate via two cheap queries. v1.5.1 —
	// /sagar leaderboard stays Bollywood-movie only (pre-global-catalog).
	rows, err := s.db.QueryContext(ctx, `
        SELECT m.id, m.title, m.year, m.poster_url,
               COALESCE(SUM(e.likes), 0) AS likes_total
        FROM movies m
        LEFT JOIN endings e ON e.movie_id = m.id
        WHERE COALESCE(m.region,'bollywood') = 'bollywood'
          AND COALESCE(m.kind,'movie')       = 'movie'
        GROUP BY m.id, m.title, m.year, m.poster_url
    `)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	type agg struct{ leader sagarLeader }
	all := []sagarLeader{}
	idx := map[string]int{}
	for rows.Next() {
		var l sagarLeader
		if err := rows.Scan(&l.ID, &l.Title, &l.Year, &l.PosterURL, &l.LikesTotal); err != nil {
			rows.Close()
			http.Error(w, err.Error(), 500)
			return
		}
		idx[l.ID] = len(all)
		all = append(all, l)
	}
	rows.Close()

	vrows, err := s.db.QueryContext(ctx, `
        SELECT movie_id, COUNT(DISTINCT anon_id)
        FROM telemetry_events
        WHERE event_type = 'card_enter' AND movie_id IS NOT NULL AND movie_id != ''
        GROUP BY movie_id
    `)
	if err == nil {
		for vrows.Next() {
			var mid string
			var v int64
			if err := vrows.Scan(&mid, &v); err == nil {
				if i, ok := idx[mid]; ok {
					all[i].Views = v
				}
			}
		}
		vrows.Close()
	}
	for i := range all {
		if all[i].Views > 0 {
			pct := float64(all[i].LikesTotal) / float64(all[i].Views) * 100.0
			if pct > 100.0 {
				pct = 100.0
			}
			all[i].ConversionPct = float64(int(pct*10+0.5)) / 10.0
		}
	}

	// Filter — only show movies with any traction.
	filtered := all[:0]
	for _, l := range all {
		if l.Views > 0 || l.LikesTotal > 0 {
			filtered = append(filtered, l)
		}
	}

	// Sort.
	switch sortBy {
	case "views":
		sortLeaderboard(filtered, func(a, b sagarLeader) bool {
			if a.Views != b.Views {
				return a.Views > b.Views
			}
			return a.LikesTotal > b.LikesTotal
		})
	case "conversion":
		sortLeaderboard(filtered, func(a, b sagarLeader) bool {
			if a.ConversionPct != b.ConversionPct {
				return a.ConversionPct > b.ConversionPct
			}
			return a.LikesTotal > b.LikesTotal
		})
	default: // likes
		sortLeaderboard(filtered, func(a, b sagarLeader) bool {
			if a.LikesTotal != b.LikesTotal {
				return a.LikesTotal > b.LikesTotal
			}
			return a.Views > b.Views
		})
	}
	for i := range filtered {
		filtered[i].Rank = i + 1
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"app_version": "1.0.0+sagar",
		"sort":        sortBy,
		"generated":   time.Now().UTC().Format(time.RFC3339),
		"count":       len(filtered),
		"leaderboard": filtered,
	})
}

// Tiny insertion-sort helper to avoid pulling in sort.Slice's reflection.
// 100 entries max → O(n²) is fine.
func sortLeaderboard(a []sagarLeader, less func(x, y sagarLeader) bool) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && less(a[j], a[j-1]); j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

// ---------- /api/sagar/flag-audio ----------
//
// Anyone can report a movie's audio as wrong. We persist it as a feedback
// row (kind='wrong-audio', extra_json=movie_id+song_query) so the existing
// dashboard's "Recent feedback" view picks it up automatically.

type sagarFlagBody struct {
	SessionID string `json:"session_id"`
	AnonID    string `json:"anon_id"`
	MovieID   string `json:"movie_id"`
	Note      string `json:"note,omitempty"`
}

func (s *Server) handleSagarFlagAudio(w http.ResponseWriter, r *http.Request) {
	var b sagarFlagBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	if b.SessionID == "" || b.AnonID == "" || b.MovieID == "" {
		http.Error(w, "session_id+anon_id+movie_id required", 400)
		return
	}
	ip := clientIP(r)
	ua := r.UserAgent()
	if len(ua) > 400 {
		ua = ua[:400]
	}
	country, _ := s.geoLookup(ip)
	songQuery, _ := s.songQueryFor(b.MovieID)
	note := strings.TrimSpace(b.Note)
	if note == "" {
		note = "wrong music"
	}
	if len(note) > 500 {
		note = note[:500]
	}
	text := "[" + b.MovieID + "] " + note + " (current query: " + songQuery + ")"
	if _, err := s.db.ExecContext(r.Context(), `
        INSERT INTO feedbacks (ts, session_id, anon_id, kind, text, ip, country, ua, mode)
        VALUES (?, ?, ?, 'wrong-audio', ?, ?, ?, ?, 'sagar')
    `, time.Now().UnixMilli(), b.SessionID, b.AnonID, text, ip, country, ua); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------- song-query lookup ----------
//
// The song pipeline writes per-movie search queries to
// data/audio-queries.json (movie_id -> "search query"). We also fall back
// to the runtime-cache copy under the session-state folder which the
// pipeline maintains during long preprocess runs. Both are best-effort:
// if neither exists we just return ""+false.

var (
	songQueryPaths = []string{
		// Repo-local copy (committed once preprocessing settles).
		`C:\Users\arushi\Rewire\data\audio-queries.json`,
		// Runtime copy maintained by song_pipeline.py (always fresh).
		`C:\Users\arushi\.copilot\session-state\abd0700f-1772-4351-81ba-707e1cdfc3db\files\rewire-runtime\audio-queries.json`,
	}
	songQueryCache    map[string]string
	songQueryLoadedAt time.Time
)

func (s *Server) songQueryFor(movieID string) (string, bool) {
	if time.Since(songQueryLoadedAt) > 60*time.Second || songQueryCache == nil {
		songQueryCache = loadSongQueries()
		songQueryLoadedAt = time.Now()
	}
	q, ok := songQueryCache[movieID]
	return q, ok
}

func loadSongQueries() map[string]string {
	for _, p := range songQueryPaths {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var m map[string]string
		if err := json.Unmarshal(raw, &m); err == nil && len(m) > 0 {
			return m
		}
	}
	return map[string]string{}
}

// ---------- frontend ----------

func (s *Server) handleSagarFrontend() http.Handler {
	dir := filepath.Join(filepath.Dir(s.frontendDir), "frontend-sagar")
	if _, err := os.Stat(dir); err != nil {
		dir = s.frontendDir
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/sagar")
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
