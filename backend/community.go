// Package main — community endings on the default `/` route.
//
// User feedback (Abhinav, post-v1.1): "I want to write my own ending and see
// what others wrote. Show how many liked it, what people said." That feature
// existed at /abhinav but was missing from /. v1.2 brings it to /.
//
// We piggy-back on the existing `abhinav_endings` table (introduced for
// /abhinav). It already has slot/source/author/likes/rating_sum/rating_n.
// Both `/` and `/abhinav` read & write the same rows, so engagement is
// shared across modes — exactly what users expect.
//
// New endpoints (no auth):
//   POST /api/community/submit-ending  — content_id, text, author, anon_id
//   POST /api/community/like-ending    — ending_id, delta (±1)
//   POST /api/community/rate-ending    — ending_id, rating (1-5), anon_id
//
// All three tag telemetry with mode='direct' so dashboards stay separable.
package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

type communitySubmitBody struct {
	ContentID string `json:"content_id"`
	Text      string `json:"text"`
	Author    string `json:"author"`
	AnonID    string `json:"anon_id"`
}

func (s *Server) handleCommunitySubmitEnding(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8*1024)
	var b communitySubmitBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	b.Text = strings.TrimSpace(b.Text)
	b.Author = strings.TrimSpace(b.Author)
	b.ContentID = strings.TrimSpace(b.ContentID)
	if b.ContentID == "" || b.Text == "" {
		http.Error(w, "content_id + text required", http.StatusBadRequest)
		return
	}
	if len([]rune(b.Text)) > 200 {
		b.Text = string([]rune(b.Text)[:200])
	}
	if b.Author == "" {
		b.Author = "anonymous"
	}
	if len(b.Author) > 32 {
		b.Author = b.Author[:32]
	}

	// Validate that content_id exists in movies OR abhinav_content.
	var ok bool
	_ = s.db.QueryRowContext(r.Context(),
		`SELECT EXISTS(SELECT 1 FROM movies WHERE id = ?)`, b.ContentID).Scan(&ok)
	if !ok {
		_ = s.db.QueryRowContext(r.Context(),
			`SELECT EXISTS(SELECT 1 FROM abhinav_content WHERE id = ?)`, b.ContentID).Scan(&ok)
	}
	if !ok {
		http.Error(w, "unknown content_id", http.StatusNotFound)
		return
	}

	// Per-anon basic rate-limit: 3 submissions per content_id per anon.
	if b.AnonID != "" {
		var n int
		_ = s.db.QueryRowContext(r.Context(),
			`SELECT COUNT(*) FROM abhinav_endings WHERE content_id = ? AND author = ?`,
			b.ContentID, b.Author).Scan(&n)
		if n >= 3 {
			http.Error(w, "you've already submitted 3 endings for this title", http.StatusTooManyRequests)
			return
		}
	}

	aiScore := scoreEnding(b.Text)

	var nextSlot int
	_ = s.db.QueryRowContext(r.Context(),
		`SELECT COALESCE(MAX(slot),0)+1 FROM abhinav_endings WHERE content_id = ?`,
		b.ContentID).Scan(&nextSlot)
	if nextSlot < 4 {
		nextSlot = 4 // 1..3 reserved for AI canon endings.
	}
	res, err := s.db.ExecContext(r.Context(), `
        INSERT INTO abhinav_endings (content_id, slot, source, author, text, ai_score)
        VALUES (?, ?, 'community', ?, ?, ?)
    `, b.ContentID, nextSlot, b.Author, b.Text, aiScore)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	id, _ := res.LastInsertId()
	s.bustCache()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "ending_id": id, "ai_score": aiScore,
		"author": b.Author, "text": b.Text,
	})
}

type communityLikeBody struct {
	EndingID int64 `json:"ending_id"`
	Delta    int64 `json:"delta"`
}

func (s *Server) handleCommunityLikeEnding(w http.ResponseWriter, r *http.Request) {
	var b communityLikeBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if b.EndingID == 0 {
		http.Error(w, "ending_id required", http.StatusBadRequest)
		return
	}
	if b.Delta == 0 {
		b.Delta = 1
	}
	if b.Delta > 1 {
		b.Delta = 1
	}
	if b.Delta < -1 {
		b.Delta = -1
	}
	if _, err := s.db.ExecContext(r.Context(),
		`UPDATE abhinav_endings SET likes = MAX(0, likes + ?) WHERE id = ?`,
		b.Delta, b.EndingID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var likes int64
	_ = s.db.QueryRowContext(r.Context(),
		`SELECT likes FROM abhinav_endings WHERE id = ?`, b.EndingID).Scan(&likes)
	s.bustCache()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "likes": likes})
}

type communityRateBody struct {
	EndingID int64  `json:"ending_id"`
	Rating   int    `json:"rating"`
	AnonID   string `json:"anon_id"`
}

func (s *Server) handleCommunityRateEnding(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4*1024)
	var b communityRateBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if b.EndingID == 0 || strings.TrimSpace(b.AnonID) == "" {
		http.Error(w, "ending_id+anon_id required", http.StatusBadRequest)
		return
	}
	if b.Rating < 1 {
		b.Rating = 1
	}
	if b.Rating > 5 {
		b.Rating = 5
	}
	now := time.Now().UnixMilli()

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	// Upsert against (ending_id, target='community', anon_id).
	var prev int
	row := tx.QueryRowContext(r.Context(),
		`SELECT rating FROM abhinav_ratings WHERE ending_id = ? AND target = 'community' AND anon_id = ?`,
		b.EndingID, b.AnonID)
	previousFound := row.Scan(&prev) == nil
	if previousFound {
		_, _ = tx.ExecContext(r.Context(),
			`UPDATE abhinav_ratings SET rating = ?, created_at = ?
              WHERE ending_id = ? AND target = 'community' AND anon_id = ?`,
			b.Rating, now, b.EndingID, b.AnonID)
		_, _ = tx.ExecContext(r.Context(),
			`UPDATE abhinav_endings SET rating_sum = rating_sum + ? WHERE id = ?`,
			float64(b.Rating-prev), b.EndingID)
	} else {
		_, _ = tx.ExecContext(r.Context(),
			`INSERT INTO abhinav_ratings (ending_id, target, anon_id, rating, created_at)
              VALUES (?, 'community', ?, ?, ?)`,
			b.EndingID, b.AnonID, b.Rating, now)
		_, _ = tx.ExecContext(r.Context(),
			`UPDATE abhinav_endings SET rating_sum = rating_sum + ?, rating_n = rating_n + 1 WHERE id = ?`,
			float64(b.Rating), b.EndingID)
	}

	if err := tx.Commit(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var avg float64
	var n int64
	_ = s.db.QueryRowContext(r.Context(),
		`SELECT CASE WHEN rating_n=0 THEN 0 ELSE rating_sum/rating_n END, rating_n
          FROM abhinav_endings WHERE id = ?`, b.EndingID).Scan(&avg, &n)
	s.bustCache()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "avg_rating": avg, "rating_n": n, "your_rating": b.Rating,
	})
}
