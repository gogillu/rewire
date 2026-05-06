// Package main — Rewire backend. Tiny, low-memory HTTP+TLS server.
//
// Hosts /api/* JSON endpoints + the static frontend. Stores movies, endings
// and likes in a single SQLite file so cold starts are instant and the whole
// process stays well under 100 MB.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

type Movie struct {
	ID            string   `json:"id"`
	Title         string   `json:"title"`
	Year          int      `json:"year"`
	IMDBRating    float64  `json:"imdb_rating"`
	Genre         string   `json:"genre"`
	Synopsis      string   `json:"synopsis"`
	ActualEnding  string   `json:"actual_ending"`
	PosterURL     string   `json:"poster_url"`
	BackdropURL   string   `json:"backdrop_url,omitempty"`
	YouTubeID     string   `json:"youtube_id,omitempty"`
	HasAudio      bool     `json:"has_audio"`
	AudioVersion  int64    `json:"audio_version,omitempty"`
	Endings       []Ending `json:"endings"`
}

type Ending struct {
	ID    int64  `json:"id"`
	Model string `json:"model"`
	Text  string `json:"text"`
	Likes int64  `json:"likes"`
}

type Server struct {
	db          *sql.DB
	frontendDir string
	dataDir     string

	cacheMu   sync.RWMutex
	cacheJSON []byte
	cacheAt   time.Time

	geo *geoCache
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS movies (
    id            TEXT PRIMARY KEY,
    title         TEXT NOT NULL,
    year          INTEGER,
    imdb_rating   REAL,
    genre         TEXT,
    synopsis      TEXT,
    actual_ending TEXT,
    poster_url    TEXT,
    backdrop_url  TEXT,
    youtube_id    TEXT,
    sort_order    INTEGER DEFAULT 0
);
CREATE TABLE IF NOT EXISTS endings (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    movie_id    TEXT NOT NULL REFERENCES movies(id) ON DELETE CASCADE,
    model       TEXT NOT NULL,
    slot        INTEGER NOT NULL,
    text        TEXT NOT NULL,
    likes       INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(movie_id, slot)
);
CREATE INDEX IF NOT EXISTS idx_endings_movie ON endings(movie_id);
`

func main() {
	port := flag.Int("port", 9999, "TLS listen port")
	plain := flag.Bool("plain", false, "serve plain HTTP (no TLS) — for debugging only")
	certDir := flag.String("certdir", `C:\Certbot\live\gogillu.in`, "directory containing fullchain.pem + privkey.pem")
	dataDir := flag.String("datadir", "data", "directory for SQLite + seed JSON")
	frontDir := flag.String("frontend", "frontend", "directory containing index.html + assets")
	flag.Parse()

	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		log.Fatalf("rewire: mkdir data: %v", err)
	}

	dbPath := filepath.Join(*dataDir, "rewire.db")
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		log.Fatalf("rewire: open db: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(8)
	if _, err := db.Exec(schemaSQL); err != nil {
		log.Fatalf("rewire: schema: %v", err)
	}
	if _, err := db.Exec(telemetrySchema); err != nil {
		log.Fatalf("rewire: telemetry schema: %v", err)
	}
	if _, err := db.Exec(abhinavSchema); err != nil {
		log.Fatalf("rewire: abhinav schema: %v", err)
	}

	srv := &Server{db: db, frontendDir: *frontDir, dataDir: *dataDir}
	srv.initGeoCache()
	srv.migrateAddModeColumn()
	if err := srv.seedAbhinavContent(); err != nil {
		log.Printf("rewire: seed abhinav: %v", err)
	}

	// Seed movies from movies.json on every boot — INSERT OR IGNORE so we
	// never clobber endings/likes that already exist for previously seeded
	// movies. Adding new movies later just appends rows.
	if err := srv.seedMovies(filepath.Join(*dataDir, "movies.json")); err != nil {
		log.Printf("rewire: seed movies: %v", err)
	}

	// Fill missing poster URLs from Wikipedia in the background so the
	// server starts answering immediately even when network is slow.
	go srv.backfillPosters(context.Background())

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", srv.handleHealth)
	mux.HandleFunc("GET /api/movies", srv.handleMovies)
	mux.HandleFunc("POST /api/like", srv.handleLike)
	mux.HandleFunc("POST /api/events", srv.handleEvents)
	mux.HandleFunc("POST /api/feedback", srv.handleFeedback)
	mux.HandleFunc("GET /api/stats", srv.handleStats)
	mux.HandleFunc("GET /dashboard", srv.handleDashboard)
	mux.HandleFunc("POST /api/admin/upsert-ending", srv.handleUpsertEnding)
	mux.HandleFunc("GET /api/abhinav/movies", srv.handleAbhinavMovies)
	mux.HandleFunc("POST /api/abhinav/like", srv.handleAbhinavLike)
	mux.HandleFunc("POST /api/abhinav/submit-ending", srv.handleAbhinavSubmitEnding)
	mux.HandleFunc("POST /api/abhinav/rate-ending", srv.handleAbhinavRateEnding)
	mux.Handle("GET /abhinav", srv.handleAbhinavFrontend())
	mux.Handle("GET /abhinav/", srv.handleAbhinavFrontend())
	mux.Handle("GET /audio/", srv.audioHandler())
	mux.Handle("/", srv.staticHandler())

	// Wrap with permissive CORS so the frontend can be embedded anywhere.
	handler := withCORS(mux)

	addr := fmt.Sprintf("0.0.0.0:%d", *port)
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
	}

	go func() {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		<-ctx.Done()
		log.Printf("rewire: shutdown signal")
		shutCtx, c2 := context.WithTimeout(context.Background(), 5*time.Second)
		defer c2()
		_ = httpSrv.Shutdown(shutCtx)
	}()

	if *plain {
		log.Printf("rewire: HTTP listen on %s (plain, debug)", addr)
		err = httpSrv.ListenAndServe()
	} else {
		certPath := filepath.Join(*certDir, "fullchain.pem")
		keyPath := filepath.Join(*certDir, "privkey.pem")
		log.Printf("rewire: HTTPS listen on %s (cert=%s)", addr, certPath)
		err = httpSrv.ListenAndServeTLS(certPath, keyPath)
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("rewire: serve: %v", err)
	}
}

// ---------- HTTP handlers ----------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	var movieN, endingN int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM movies`).Scan(&movieN)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM endings`).Scan(&endingN)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"movies":  movieN,
		"endings": endingN,
		"time":    time.Now().UTC().Format(time.RFC3339),
	})
}

// handleMovies returns the full movie payload. We cache the JSON for 5
// seconds in-process — the frontend asks for the whole list once on load
// and every "like" invalidates the cache so counts stay live without a
// per-row roundtrip.
func (s *Server) handleMovies(w http.ResponseWriter, r *http.Request) {
	s.cacheMu.RLock()
	cached := s.cacheJSON
	fresh := time.Since(s.cacheAt) < 5*time.Second
	s.cacheMu.RUnlock()
	if fresh && cached != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=10")
		_, _ = w.Write(cached)
		return
	}

	movies, err := s.loadMovies(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Optional shuffle seed via ?seed= so the client can re-shuffle without
	// the server having to track sessions. Default = stable IMDB order.
	if seed := r.URL.Query().Get("seed"); seed != "" {
		shuffleStable(movies, hashStr(seed))
	}

	body, _ := json.Marshal(map[string]any{
		"movies":      movies,
		"generated":   time.Now().UTC().Format(time.RFC3339),
		"count":       len(movies),
		"app_version": "0.1.0",
	})
	s.cacheMu.Lock()
	s.cacheJSON = body
	s.cacheAt = time.Now()
	s.cacheMu.Unlock()

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=10")
	_, _ = w.Write(body)
}

func (s *Server) handleLike(w http.ResponseWriter, r *http.Request) {
	var body struct {
		EndingID int64 `json:"ending_id"`
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
	if body.Delta == 0 {
		body.Delta = 1
	}
	// Clamp to ±1; this is a like counter, not a free-form integer.
	if body.Delta > 1 {
		body.Delta = 1
	}
	if body.Delta < -1 {
		body.Delta = -1
	}
	if _, err := s.db.ExecContext(r.Context(),
		`UPDATE endings SET likes = MAX(0, likes + ?) WHERE id = ?`,
		body.Delta, body.EndingID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var likes int64
	_ = s.db.QueryRowContext(r.Context(),
		`SELECT likes FROM endings WHERE id = ?`, body.EndingID).Scan(&likes)
	s.bustCache()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "likes": likes})
}

// handleUpsertEnding is used by the preprocess job (god backend → here).
// Auth via X-Rewire-Admin header matched against env GOD_REWIRE_ADMIN.
func (s *Server) handleUpsertEnding(w http.ResponseWriter, r *http.Request) {
	want := os.Getenv("REWIRE_ADMIN")
	got := r.Header.Get("X-Rewire-Admin")
	if want == "" || got != want {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var body struct {
		MovieID string `json:"movie_id"`
		Slot    int    `json:"slot"`
		Model   string `json:"model"`
		Text    string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if body.MovieID == "" || body.Slot < 1 || body.Slot > 3 || strings.TrimSpace(body.Text) == "" {
		http.Error(w, "movie_id + slot(1-3) + text required", http.StatusBadRequest)
		return
	}
	_, err := s.db.ExecContext(r.Context(), `
        INSERT INTO endings (movie_id, slot, model, text)
        VALUES (?, ?, ?, ?)
        ON CONFLICT(movie_id, slot) DO UPDATE SET
            model = excluded.model,
            text  = excluded.text
    `, body.MovieID, body.Slot, body.Model, strings.TrimSpace(body.Text))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.bustCache()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) bustCache() {
	s.cacheMu.Lock()
	s.cacheJSON = nil
	s.cacheAt = time.Time{}
	s.cacheMu.Unlock()
}

// ---------- DB helpers ----------

func (s *Server) loadMovies(ctx context.Context) ([]Movie, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT id, title, year, imdb_rating, genre, synopsis,
               actual_ending, poster_url, backdrop_url, youtube_id
        FROM movies
        ORDER BY sort_order ASC, imdb_rating DESC, year DESC
    `)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Movie
	idx := map[string]int{}
	for rows.Next() {
		var m Movie
		if err := rows.Scan(&m.ID, &m.Title, &m.Year, &m.IMDBRating, &m.Genre,
			&m.Synopsis, &m.ActualEnding, &m.PosterURL, &m.BackdropURL, &m.YouTubeID); err != nil {
			return nil, err
		}
		m.Endings = []Ending{}
		// has_audio is computed from disk; cheap stat call.
		if info, err := os.Stat(filepath.Join(s.dataDir, "audio", m.ID+".mp3")); err == nil && info.Size() > 50_000 {
			m.HasAudio = true
			m.AudioVersion = info.ModTime().Unix()
		}
		idx[m.ID] = len(out)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	erows, err := s.db.QueryContext(ctx, `
        SELECT id, movie_id, model, text, likes, slot
        FROM endings
        ORDER BY movie_id, slot
    `)
	if err != nil {
		return nil, err
	}
	defer erows.Close()
	for erows.Next() {
		var e Ending
		var movieID string
		var slot int
		if err := erows.Scan(&e.ID, &movieID, &e.Model, &e.Text, &e.Likes, &slot); err != nil {
			return nil, err
		}
		_ = slot
		if i, ok := idx[movieID]; ok {
			out[i].Endings = append(out[i].Endings, e)
		}
	}
	return out, erows.Err()
}

// seedMovies upserts every movie in the JSON file. Existing rows keep their
// likes (rows in `endings` survive because we only touch `movies` here).
func (s *Server) seedMovies(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var movies []Movie
	if err := json.Unmarshal(raw, &movies); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`
        INSERT INTO movies (id, title, year, imdb_rating, genre, synopsis,
                            actual_ending, poster_url, backdrop_url, youtube_id, sort_order)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(id) DO UPDATE SET
            title=excluded.title, year=excluded.year, imdb_rating=excluded.imdb_rating,
            genre=excluded.genre, synopsis=excluded.synopsis,
            actual_ending=excluded.actual_ending, poster_url=excluded.poster_url,
            backdrop_url=excluded.backdrop_url, youtube_id=excluded.youtube_id,
            sort_order=excluded.sort_order
    `)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for i, m := range movies {
		if _, err := stmt.Exec(m.ID, m.Title, m.Year, m.IMDBRating, m.Genre, m.Synopsis,
			m.ActualEnding, m.PosterURL, m.BackdropURL, m.YouTubeID, i); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	log.Printf("rewire: seeded %d movies", len(movies))
	s.bustCache()
	return nil
}

// ---------- static + middleware ----------

func (s *Server) staticHandler() http.Handler {
	fs := http.FileServer(http.Dir(s.frontendDir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// SPA fallback: anything that's not a real file goes to index.html.
		full := filepath.Join(s.frontendDir, filepath.FromSlash(strings.TrimPrefix(r.URL.Path, "/")))
		if r.URL.Path == "/" {
			full = filepath.Join(s.frontendDir, "index.html")
		}
		if info, err := os.Stat(full); err != nil || info.IsDir() {
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/index.html"
			fs.ServeHTTP(w, r2)
			return
		}
		// Long cache for hashed assets, short cache for HTML.
		if strings.HasSuffix(r.URL.Path, ".html") || r.URL.Path == "/" {
			w.Header().Set("Cache-Control", "no-cache")
		} else if strings.HasSuffix(r.URL.Path, ".js") || strings.HasSuffix(r.URL.Path, ".css") {
			w.Header().Set("Cache-Control", "public, max-age=300")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=86400")
		}
		fs.ServeHTTP(w, r)
	})
}

// audioHandler streams /audio/<movie_id>.mp3 from data/audio/. Range
// requests are handled by http.ServeFile so the browser can scrub /
// resume on flaky networks. Cached aggressively in the SW.
func (s *Server) audioHandler() http.Handler {
	dir := filepath.Join(s.dataDir, "audio")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// strip "/audio/" prefix; only serve <id>.mp3 files inside data/audio/
		name := strings.TrimPrefix(r.URL.Path, "/audio/")
		if name == "" || strings.ContainsAny(name, `/\`) || !strings.HasSuffix(name, ".mp3") {
			http.NotFound(w, r)
			return
		}
		full := filepath.Join(dir, name)
		info, err := os.Stat(full)
		if err != nil || info.IsDir() {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Cache-Control", "public, max-age=2592000, immutable")
		w.Header().Set("Accept-Ranges", "bytes")
		http.ServeFile(w, r, full)
	})
}

func withCORS(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Rewire-Admin")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ---------- helpers ----------

func hashStr(s string) uint64 {
	var h uint64 = 1469598103934665603
	for _, b := range []byte(s) {
		h ^= uint64(b)
		h *= 1099511628211
	}
	return h
}

func shuffleStable(m []Movie, seed uint64) {
	r := rand.New(rand.NewPCG(seed, seed^0x9E3779B97F4A7C15))
	r.Shuffle(len(m), func(i, j int) { m[i], m[j] = m[j], m[i] })
}

// envLookup is a thin wrapper so telemetry.go can read env without importing
// "os" twice in inline helpers.
func envLookup(k string) string { return os.Getenv(k) }
