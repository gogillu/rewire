// Package main — vibe-tagged endings (Premium "Intense Mode").
//
// Lives in a separate table from the classic 3-ending set so:
//   - Free users keep seeing the original 3 endings only (zero risk of
//     leak — /api/movies and the per-movie classic queries never touch
//     vibe_endings).
//   - Premium users get a deep pool of 6 vibes × N variants per movie.
//   - The pre-generation pipeline is fully resumable: a UNIQUE
//     (movie_id, vibe, variant) constraint plus an idempotent upsert
//     means the script can be killed and restarted forever.
package main

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"
)

const vibeSchema = `
CREATE TABLE IF NOT EXISTS vibe_endings (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    movie_id    TEXT NOT NULL,
    vibe        TEXT NOT NULL,
    variant     INTEGER NOT NULL,
    model       TEXT,
    text        TEXT NOT NULL,
    fp          TEXT,                       -- short hash for cross-vibe dedupe
    likes       INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    UNIQUE(movie_id, vibe, variant)
);
CREATE INDEX IF NOT EXISTS idx_ve_movie ON vibe_endings(movie_id);
CREATE INDEX IF NOT EXISTS idx_ve_vibe  ON vibe_endings(vibe);
CREATE INDEX IF NOT EXISTS idx_ve_likes ON vibe_endings(likes DESC);
`

// Allowed vibe set. Changes here must be mirrored in the pre-generation
// pipeline and the /premium frontend.
var allowedVibes = map[string]bool{
	"humour":        true,
	"emotional":     true,
	"controversial": true,
	"sad":           true,
	"happy":         true,
	"impossible":    true,
}

func validVibe(v string) bool { return allowedVibes[strings.ToLower(strings.TrimSpace(v))] }

type vibeUpsertBody struct {
	MovieID string `json:"movie_id"`
	Vibe    string `json:"vibe"`
	Variant int    `json:"variant"`
	Model   string `json:"model"`
	Text    string `json:"text"`
}

// handleAdminVibeUpsert — auth via X-Rewire-Admin header. Used by the
// vibe pre-generation pipeline to push generated endings into the DB.
// Idempotent: re-pushing the same (movie_id, vibe, variant) replaces
// the text + model.
func (s *Server) handleAdminVibeUpsert(w http.ResponseWriter, r *http.Request) {
	want := strings.TrimSpace(os.Getenv("REWIRE_ADMIN"))
	got := r.Header.Get("X-Rewire-Admin")
	if want == "" || got != want {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var b vibeUpsertBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	b.Vibe = strings.ToLower(strings.TrimSpace(b.Vibe))
	b.Text = strings.TrimSpace(b.Text)
	if b.MovieID == "" || !validVibe(b.Vibe) || b.Variant < 1 || b.Variant > 8 || b.Text == "" {
		http.Error(w, "movie_id+vibe+variant(1-8)+text required", http.StatusBadRequest)
		return
	}
	if len(b.Text) > 220 {
		b.Text = b.Text[:220]
	}
	now := time.Now().UnixMilli()
	fp := shortFingerprint(b.Text)
	_, err := s.db.ExecContext(r.Context(), `
        INSERT INTO vibe_endings (movie_id, vibe, variant, model, text, fp, created_at, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(movie_id, vibe, variant) DO UPDATE SET
            model=excluded.model,
            text=excluded.text,
            fp=excluded.fp,
            updated_at=excluded.updated_at
    `, b.MovieID, b.Vibe, b.Variant, b.Model, b.Text, fp, now, now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// shortFingerprint returns a 12-hex-char hash of the lowercased text, used
// to suppress near-identical endings across vibes for the same movie.
func shortFingerprint(t string) string {
	t = strings.ToLower(strings.Join(strings.Fields(t), " "))
	h := sha1.Sum([]byte(t))
	return hex.EncodeToString(h[:6])
}

// loadVibeEndings returns vibe-tagged endings for a movie filtered by the
// caller's selected vibe set. Empty `vibes` slice means "all vibes".
func (s *Server) loadVibeEndings(ctx context.Context, movieID string, vibes []string) ([]Ending, []string, error) {
	q := `SELECT id, vibe, model, text, likes FROM vibe_endings WHERE movie_id = ?`
	args := []any{movieID}
	if len(vibes) > 0 {
		placeholders := strings.Repeat(",?", len(vibes))[1:]
		q += ` AND vibe IN (` + placeholders + `)`
		for _, v := range vibes {
			args = append(args, v)
		}
	}
	q += ` ORDER BY likes DESC, id ASC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out []Ending
	var outVibes []string
	for rows.Next() {
		var e Ending
		var vibe string
		var model sql.NullString
		if err := rows.Scan(&e.ID, &vibe, &model, &e.Text, &e.Likes); err != nil {
			return nil, nil, err
		}
		if model.Valid {
			e.Model = model.String
		}
		out = append(out, e)
		outVibes = append(outVibes, vibe)
	}
	return out, outVibes, rows.Err()
}
