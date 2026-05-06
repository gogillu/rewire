// Package main — telemetry + feedback ingestion.
//
// All client-emitted events land here. We persist a flat row per event so
// the dashboard can slice arbitrarily (per-movie, per-country, per-device).
// Personally-identifying data is limited to anon_id (random uuid created on
// the client and stored in localStorage) + IP (hashed in the dashboard).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const telemetrySchema = `
CREATE TABLE IF NOT EXISTS telemetry_events (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    ts            INTEGER NOT NULL,
    session_id    TEXT NOT NULL,
    anon_id       TEXT NOT NULL,
    event_type    TEXT NOT NULL,
    movie_id      TEXT,
    ending_id     INTEGER,
    duration_ms   INTEGER,
    audio_playing INTEGER DEFAULT 0,
    ip            TEXT,
    country       TEXT,
    city          TEXT,
    os            TEXT,
    browser       TEXT,
    device        TEXT,
    screen_w      INTEGER,
    screen_h      INTEGER,
    ua            TEXT,
    extra_json    TEXT
);
CREATE INDEX IF NOT EXISTS idx_te_ts    ON telemetry_events(ts);
CREATE INDEX IF NOT EXISTS idx_te_movie ON telemetry_events(movie_id);
CREATE INDEX IF NOT EXISTS idx_te_anon  ON telemetry_events(anon_id);
CREATE INDEX IF NOT EXISTS idx_te_type  ON telemetry_events(event_type);

CREATE TABLE IF NOT EXISTS feedbacks (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    ts         INTEGER NOT NULL,
    session_id TEXT NOT NULL,
    anon_id    TEXT NOT NULL,
    kind       TEXT NOT NULL,
    text       TEXT,
    ip         TEXT,
    country    TEXT,
    ua         TEXT
);
CREATE INDEX IF NOT EXISTS idx_fb_ts ON feedbacks(ts);

CREATE TABLE IF NOT EXISTS ip_geo (
    ip          TEXT PRIMARY KEY,
    country     TEXT,
    region      TEXT,
    city        TEXT,
    isp         TEXT,
    resolved_at INTEGER NOT NULL
);
`

type clientEvent struct {
	Ts           int64           `json:"ts"`
	Type         string          `json:"type"`
	MovieID      string          `json:"movie_id,omitempty"`
	EndingID     int64           `json:"ending_id,omitempty"`
	DurationMs   int64           `json:"duration_ms,omitempty"`
	AudioPlaying int             `json:"audio_playing,omitempty"`
	Extra        json.RawMessage `json:"extra,omitempty"`
}

type eventBatch struct {
	SessionID string        `json:"session_id"`
	AnonID    string        `json:"anon_id"`
	OS        string        `json:"os"`
	Browser   string        `json:"browser"`
	Device    string        `json:"device"`
	ScreenW   int           `json:"screen_w"`
	ScreenH   int           `json:"screen_h"`
	Mode      string        `json:"mode"`
	Events    []clientEvent `json:"events"`
}

type feedbackBody struct {
	SessionID string `json:"session_id"`
	AnonID    string `json:"anon_id"`
	Kind      string `json:"kind"` // interesting|stupid|custom
	Text      string `json:"text,omitempty"`
	Mode      string `json:"mode,omitempty"`
}

// handleEvents accepts a batch of events from one client, persists them,
// and triggers an async IP→country lookup if the IP isn't cached yet.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 256*1024)
	var b eventBatch
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if b.SessionID == "" || b.AnonID == "" || len(b.Events) == 0 {
		http.Error(w, "session_id+anon_id+events required", http.StatusBadRequest)
		return
	}
	if len(b.Events) > 200 {
		b.Events = b.Events[:200]
	}
	mode := strings.ToLower(strings.TrimSpace(b.Mode))
	if mode != "abhinav" {
		mode = "direct"
	}
	ip := clientIP(r)
	ua := r.UserAgent()
	if len(ua) > 400 {
		ua = ua[:400]
	}
	country, city := s.geoLookup(ip) // may be empty if not yet resolved

	tx, err := s.db.Begin()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`
        INSERT INTO telemetry_events (
            ts, session_id, anon_id, event_type, movie_id, ending_id,
            duration_ms, audio_playing, ip, country, city, os, browser,
            device, screen_w, screen_h, ua, extra_json, mode
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    `)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer stmt.Close()
	now := time.Now().UnixMilli()
	for _, ev := range b.Events {
		ts := ev.Ts
		if ts <= 0 || ts > now+5*60_000 {
			ts = now
		}
		etype := ev.Type
		if len(etype) > 32 {
			etype = etype[:32]
		}
		movie := ev.MovieID
		if len(movie) > 64 {
			movie = movie[:64]
		}
		var extra string
		if len(ev.Extra) > 0 && len(ev.Extra) < 4096 {
			extra = string(ev.Extra)
		}
		if _, err := stmt.Exec(
			ts, b.SessionID, b.AnonID, etype, movie, ev.EndingID,
			ev.DurationMs, ev.AudioPlaying, ip, country, city, b.OS, b.Browser,
			b.Device, b.ScreenW, b.ScreenH, ua, extra, mode,
		); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Kick off geo resolution in background if needed.
	if country == "" && ip != "" && isPublicIP(ip) {
		s.geoResolveAsync(ip)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleFeedback(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	var f feedbackBody
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	f.Kind = strings.ToLower(strings.TrimSpace(f.Kind))
	switch f.Kind {
	case "interesting", "stupid", "custom", "love", "boring":
	default:
		http.Error(w, "kind must be interesting|stupid|custom", http.StatusBadRequest)
		return
	}
	if f.SessionID == "" || f.AnonID == "" {
		http.Error(w, "session_id+anon_id required", http.StatusBadRequest)
		return
	}
	if len(f.Text) > 2000 {
		f.Text = f.Text[:2000]
	}
	mode := strings.ToLower(strings.TrimSpace(f.Mode))
	if mode != "abhinav" {
		mode = "direct"
	}
	ip := clientIP(r)
	ua := r.UserAgent()
	if len(ua) > 400 {
		ua = ua[:400]
	}
	country, _ := s.geoLookup(ip)

	if _, err := s.db.ExecContext(r.Context(), `
        INSERT INTO feedbacks (ts, session_id, anon_id, kind, text, ip, country, ua, mode)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
    `, time.Now().UnixMilli(), f.SessionID, f.AnonID, f.Kind, f.Text, ip, country, ua, mode); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if country == "" && ip != "" && isPublicIP(ip) {
		s.geoResolveAsync(ip)
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- helpers ----------

func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		// X-Forwarded-For may be a comma-separated chain; the leftmost is
		// the original client.
		parts := strings.Split(v, ",")
		ip := strings.TrimSpace(parts[0])
		if ip != "" {
			return ip
		}
	}
	if v := r.Header.Get("CF-Connecting-IP"); v != "" {
		return v
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func isPublicIP(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	if parsed.IsLoopback() || parsed.IsPrivate() || parsed.IsLinkLocalUnicast() ||
		parsed.IsUnspecified() || parsed.IsMulticast() {
		return false
	}
	return true
}

// ---------- ip geo cache ----------

type geoEntry struct {
	country, region, city, isp string
}

type geoCache struct {
	mu   sync.RWMutex
	mem  map[string]geoEntry
	in   chan string
	once sync.Once
}

func (s *Server) initGeoCache() {
	s.geo = &geoCache{
		mem: map[string]geoEntry{},
		in:  make(chan string, 256),
	}
	// Warm in-mem cache from DB.
	rows, err := s.db.Query(`SELECT ip, country, region, city, isp FROM ip_geo`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var ip, c, r, ci, isp string
			if rows.Scan(&ip, &c, &r, &ci, &isp) == nil {
				s.geo.mem[ip] = geoEntry{c, r, ci, isp}
			}
		}
	}
	go s.geoResolverLoop()
}

func (s *Server) geoLookup(ip string) (country, city string) {
	if s.geo == nil || ip == "" {
		return
	}
	s.geo.mu.RLock()
	defer s.geo.mu.RUnlock()
	if e, ok := s.geo.mem[ip]; ok {
		return e.country, e.city
	}
	return
}

// geoResolveAsync schedules a non-blocking lookup. Drops if the queue is full.
func (s *Server) geoResolveAsync(ip string) {
	if s.geo == nil {
		return
	}
	select {
	case s.geo.in <- ip:
	default:
	}
}

// geoResolverLoop pulls IPs from `in`, batches them every 2s (up to 100),
// calls ip-api.com (free tier: 45 req/min, 100 IPs per request — i.e. up to
// 4500 IPs/min), then writes back to the in-mem map and sqlite cache.
func (s *Server) geoResolverLoop() {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	pending := make(map[string]struct{}, 128)
	for {
		select {
		case ip := <-s.geo.in:
			pending[ip] = struct{}{}
		case <-tick.C:
			if len(pending) == 0 {
				continue
			}
			ips := make([]string, 0, len(pending))
			for ip := range pending {
				ips = append(ips, ip)
				if len(ips) >= 100 {
					break
				}
			}
			for _, ip := range ips {
				delete(pending, ip)
			}
			s.resolveBatch(ips)
		}
	}
}

func (s *Server) resolveBatch(ips []string) {
	if len(ips) == 0 {
		return
	}
	body, _ := json.Marshal(toQuery(ips))
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST",
		"http://ip-api.com/batch?fields=status,country,regionName,city,isp,query", strings.NewReader(string(body)))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	var out []struct {
		Status, Country, RegionName, City, ISP, Query string
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return
	}
	now := time.Now().Unix()
	tx, err := s.db.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()
	stmt, _ := tx.Prepare(`
        INSERT INTO ip_geo (ip, country, region, city, isp, resolved_at)
        VALUES (?, ?, ?, ?, ?, ?)
        ON CONFLICT(ip) DO UPDATE SET
            country=excluded.country, region=excluded.region,
            city=excluded.city, isp=excluded.isp, resolved_at=excluded.resolved_at
    `)
	defer stmt.Close()
	s.geo.mu.Lock()
	for _, e := range out {
		if e.Status != "success" {
			continue
		}
		s.geo.mem[e.Query] = geoEntry{e.Country, e.RegionName, e.City, e.ISP}
		_, _ = stmt.Exec(e.Query, e.Country, e.RegionName, e.City, e.ISP, now)
	}
	s.geo.mu.Unlock()
	_ = tx.Commit()
}

func toQuery(ips []string) []map[string]string {
	out := make([]map[string]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, map[string]string{"query": ip})
	}
	return out
}

// adminAuth gates dashboard / stats endpoints. Token comes from
// REWIRE_ADMIN env var. Accept it via ?token= query, X-Rewire-Admin header,
// or "rewire_admin" cookie (set by the dashboard page after first auth).
func (s *Server) adminAuth(r *http.Request) error {
	want := strings.TrimSpace(getenvDefault("REWIRE_ADMIN", ""))
	if want == "" {
		return errors.New("admin disabled (REWIRE_ADMIN unset)")
	}
	if v := r.URL.Query().Get("token"); v != "" && v == want {
		return nil
	}
	if v := r.Header.Get("X-Rewire-Admin"); v != "" && v == want {
		return nil
	}
	if c, err := r.Cookie("rewire_admin"); err == nil && c.Value == want {
		return nil
	}
	return errors.New("forbidden")
}

func getenvDefault(k, def string) string {
	if v := strings.TrimSpace(envLookup(k)); v != "" {
		return v
	}
	return def
}
