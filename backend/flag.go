// Package main — flag report ingestion (replaces the toggle-flag UX).
//
// Three reasons accepted: wrong_audio | wrong_poster | other. Once a user
// has flagged a (movie,reason) tuple within the last 24 h we silently no-op
// so re-tapping just shows them a "concerns already shared" toast on the
// client without producing duplicate rows.
package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

const flagSchema = `
CREATE TABLE IF NOT EXISTS flag_reports (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    ts          INTEGER NOT NULL,
    session_id  TEXT NOT NULL,
    anon_id     TEXT NOT NULL,
    mode        TEXT NOT NULL DEFAULT 'direct',
    movie_id    TEXT NOT NULL,
    reason      TEXT NOT NULL,
    custom_text TEXT,
    ip          TEXT,
    country     TEXT,
    city        TEXT,
    isp         TEXT,
    ua          TEXT
);
CREATE INDEX IF NOT EXISTS idx_flag_anon  ON flag_reports(anon_id, movie_id, reason, mode);
CREATE INDEX IF NOT EXISTS idx_flag_movie ON flag_reports(movie_id);
CREATE INDEX IF NOT EXISTS idx_flag_ts    ON flag_reports(ts);
`

type flagBody struct {
	SessionID  string `json:"session_id"`
	AnonID     string `json:"anon_id"`
	Mode       string `json:"mode,omitempty"`
	MovieID    string `json:"movie_id"`
	Reason     string `json:"reason"`
	CustomText string `json:"custom_text,omitempty"`
}

func validFlagReason(r string) bool {
	switch r {
	case "wrong_audio", "wrong_poster", "other":
		return true
	}
	return false
}

// handleFlag — POST /api/flag. Always returns 200 with {ok, deduped}. Server
// dedupes on (anon_id, movie_id, reason, mode) within 24 h so accidental
// double-taps don't create noise.
func (s *Server) handleFlag(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	var b flagBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	b.Reason = strings.ToLower(strings.TrimSpace(b.Reason))
	if b.SessionID == "" || b.AnonID == "" || b.MovieID == "" || !validFlagReason(b.Reason) {
		http.Error(w, "session_id+anon_id+movie_id+reason required", http.StatusBadRequest)
		return
	}
	mode := normalizeMode(b.Mode)
	custom := strings.TrimSpace(b.CustomText)
	if len(custom) > 1000 {
		custom = custom[:1000]
	}

	// Dedupe — same (anon, movie, reason, mode) within 24 h is silently kept
	// as the original report.
	since := time.Now().Add(-24 * time.Hour).UnixMilli()
	var existing int
	_ = s.db.QueryRowContext(r.Context(), `
        SELECT COUNT(*) FROM flag_reports
        WHERE anon_id=? AND movie_id=? AND reason=? AND mode=? AND ts >= ?
    `, b.AnonID, b.MovieID, b.Reason, mode, since).Scan(&existing)
	if existing > 0 {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deduped": true})
		return
	}

	ip := clientIP(r)
	ua := r.UserAgent()
	if len(ua) > 400 {
		ua = ua[:400]
	}
	country, city, isp := s.geoLookupRich(ip)

	if _, err := s.db.ExecContext(r.Context(), `
        INSERT INTO flag_reports
            (ts, session_id, anon_id, mode, movie_id, reason, custom_text, ip, country, city, isp, ua)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    `, time.Now().UnixMilli(), b.SessionID, b.AnonID, mode, b.MovieID, b.Reason,
		custom, ip, country, city, isp, ua); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if country == "" && ip != "" && isPublicIP(ip) {
		s.geoResolveAsync(ip)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deduped": false})
}

// normalizeMode is the v1.1 mode allow-list — keeps telemetry separable.
func normalizeMode(m string) string {
	m = strings.ToLower(strings.TrimSpace(m))
	switch m {
	case "abhinav", "sagar", "premium", "v1.0.0", "buy":
		return m
	}
	return "direct"
}
