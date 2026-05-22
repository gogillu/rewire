package main

// guard.go - lightweight per-IP rate-limit + ban shield for rewire.
//
// Goals:
//   1. Cap requests-per-second per source IP using a token bucket
//      (default 60 refill/s, 120 burst).
//   2. If an IP exceeds 30 requests in a sliding 60s window, ban it
//      for 15 minutes (configurable). Banned IPs get 429 with a
//      Retry-After header.
//   3. Login bucket - the few endpoints that accept tokens or admin
//      credentials get a tighter, separate bucket (5 burst, 1/s).
//   4. Emit incidents to god backend's /v1/admin/incidents/ingest so
//      the centralized alerter can route them to WhatsApp + email.
//      Non-fatal: if god backend is down, just log.
//   5. Admin kill-switches: stats / disable / enable / unban / clear.
//      Bearer = REWIRE_ADMIN_TOKEN (must be set; refuses without).
//
// This is a self-contained shield - no god backend dependency at the
// hot path. Sends incidents fire-and-forget over a small worker queue
// so a slow god backend can never stall a rewire request.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	rewireBanCookieMarker = "rewire_banned"
)

type rewireGuardConfig struct {
	RPSRefill        int
	RPSBurst         int
	LoginRPSRefill   int
	LoginRPSBurst    int
	BanThreshold     int
	BanWindow        time.Duration
	BanDuration      time.Duration
	IncidentURL      string
	IncidentToken    string
	AdminToken       string
	ExemptCIDRs      []*net.IPNet
}

func loadRewireGuardConfig() rewireGuardConfig {
	cfg := rewireGuardConfig{
		RPSRefill:      atoiEnvOr("REWIRE_GUARD_RPS_REFILL", 10),
		RPSBurst:       atoiEnvOr("REWIRE_GUARD_RPS_BURST", 30),
		LoginRPSRefill: atoiEnvOr("REWIRE_GUARD_LOGIN_REFILL", 1),
		LoginRPSBurst:  atoiEnvOr("REWIRE_GUARD_LOGIN_BURST", 5),
		BanThreshold:   atoiEnvOr("REWIRE_GUARD_BAN_THRESHOLD", 5),
		BanWindow:      time.Duration(atoiEnvOr("REWIRE_GUARD_BAN_WINDOW_SEC", 60)) * time.Second,
		BanDuration:    time.Duration(atoiEnvOr("REWIRE_GUARD_BAN_DURATION_SEC", 900)) * time.Second,
		IncidentURL:    strings.TrimSpace(os.Getenv("GOD_BACKEND_INCIDENT_URL")),
		IncidentToken:  strings.TrimSpace(os.Getenv("GOD_EXTERNAL_INGEST_TOKEN")),
		AdminToken:     strings.TrimSpace(os.Getenv("REWIRE_ADMIN_TOKEN")),
	}
	if cfg.IncidentURL == "" {
		cfg.IncidentURL = "https://127.0.0.1:9871/v1/admin/incidents/ingest"
	}
	// Fallback chain for admin token - reuse existing REWIRE_ADMIN if no
	// dedicated REWIRE_ADMIN_TOKEN is set.
	if cfg.AdminToken == "" {
		cfg.AdminToken = strings.TrimSpace(os.Getenv("REWIRE_ADMIN"))
	}
	// Fallback for incident token - read from god backend's local token file.
	if cfg.IncidentToken == "" {
		if b, err := readFirstLine(`C:\Users\arushi\god\data\external-ingest-token.txt`); err == nil {
			cfg.IncidentToken = b
		}
	}
	// Always exempt loopback (proxy/frontproxy may forward via it).
	for _, c := range []string{"127.0.0.0/8", "::1/128", "10.0.0.0/8", "100.64.0.0/10"} {
		if _, n, err := net.ParseCIDR(c); err == nil {
			cfg.ExemptCIDRs = append(cfg.ExemptCIDRs, n)
		}
	}
	return cfg
}

func readFirstLine(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s, nil
}

func atoiEnvOr(name string, def int) int {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

type bucket struct {
	tokens   float64
	last     time.Time
	hits     []time.Time
	banUntil time.Time
}

type rewireGuard struct {
	cfg       rewireGuardConfig
	mu        sync.Mutex
	all       map[string]*bucket
	login     map[string]*bucket
	disabled  atomic.Bool
	bannedNow atomic.Int64
	dropped   atomic.Uint64
	banned    atomic.Uint64
	allowed   atomic.Uint64
	incidents chan rewireIncident
}

type rewireIncident struct {
	Kind     string `json:"kind"`
	IP       string `json:"ip"`
	UA       string `json:"ua"`
	Path     string `json:"path"`
	Method   string `json:"method"`
	Status   int    `json:"status"`
	Reason   string `json:"reason"`
	Detail   string `json:"detail,omitempty"`
	Severity string `json:"severity"`
	Source   string `json:"source"`
	TS       int64  `json:"ts"`
}

func newRewireGuard(ctx context.Context) *rewireGuard {
	g := &rewireGuard{
		cfg:       loadRewireGuardConfig(),
		all:       make(map[string]*bucket),
		login:     make(map[string]*bucket),
		incidents: make(chan rewireIncident, 256),
	}
	go g.gc(ctx)
	go g.shipIncidents(ctx)
	log.Printf("rewire-guard: rps_burst=%d rps_refill=%d ban_threshold=%d ban_window=%s ban_duration=%s admin_token=%s incident_url=%s",
		g.cfg.RPSBurst, g.cfg.RPSRefill, g.cfg.BanThreshold, g.cfg.BanWindow, g.cfg.BanDuration,
		map[bool]string{true: "set", false: "UNSET (admin disabled)"}[g.cfg.AdminToken != ""],
		g.cfg.IncidentURL,
	)
	return g
}

func (g *rewireGuard) gc(ctx context.Context) {
	t := time.NewTicker(2 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.mu.Lock()
			now := time.Now()
			for k, b := range g.all {
				if !b.banUntil.IsZero() && now.After(b.banUntil) && len(b.hits) == 0 && now.Sub(b.last) > 10*time.Minute {
					delete(g.all, k)
				}
			}
			for k, b := range g.login {
				if now.Sub(b.last) > 10*time.Minute {
					delete(g.login, k)
				}
			}
			g.refreshBannedCount()
			g.mu.Unlock()
		}
	}
}

func (g *rewireGuard) refreshBannedCount() {
	now := time.Now()
	cnt := int64(0)
	for _, b := range g.all {
		if !b.banUntil.IsZero() && now.Before(b.banUntil) {
			cnt++
		}
	}
	g.bannedNow.Store(cnt)
}

func (g *rewireGuard) shipIncidents(ctx context.Context) {
	if g.cfg.IncidentURL == "" || g.cfg.IncidentToken == "" {
		// Drain channel without HTTP if not configured.
		for {
			select {
			case <-ctx.Done():
				return
			case <-g.incidents:
				// drop
			}
		}
	}
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: insecureClientTransport(),
	}
	for {
		select {
		case <-ctx.Done():
			return
		case inc := <-g.incidents:
			body, _ := json.Marshal(inc)
			req, err := http.NewRequestWithContext(ctx, "POST", g.cfg.IncidentURL, strings.NewReader(string(body)))
			if err != nil {
				continue
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+g.cfg.IncidentToken)
			resp, err := client.Do(req)
			if err != nil {
				log.Printf("rewire-guard: incident POST failed: %v", err)
				continue
			}
			_ = resp.Body.Close()
		}
	}
}

func (g *rewireGuard) emit(kind, ip, ua, path, method, reason, detail, severity string, status int) {
	select {
	case g.incidents <- rewireIncident{
		Kind: kind, IP: ip, UA: ua, Path: path, Method: method,
		Status: status, Reason: reason, Detail: detail, Severity: severity,
		Source: "rewire", TS: time.Now().Unix(),
	}:
	default:
		// Channel full - drop. Better to lose an alert than block a hot path.
	}
}

func (g *rewireGuard) isExempt(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, n := range g.cfg.ExemptCIDRs {
		if n.Contains(parsed) {
			return true
		}
	}
	return false
}

func rewireClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if host == "127.0.0.1" || host == "::1" {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			last := strings.TrimSpace(parts[len(parts)-1])
			if last != "" {
				return last
			}
		}
	}
	return host
}

func (g *rewireGuard) take(tab map[string]*bucket, ip string, burst, refill int) bool {
	now := time.Now()
	b := tab[ip]
	if b == nil {
		b = &bucket{tokens: float64(burst), last: now}
		tab[ip] = b
	}
	elapsed := now.Sub(b.last).Seconds()
	b.tokens += elapsed * float64(refill)
	if b.tokens > float64(burst) {
		b.tokens = float64(burst)
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// recordNaughtyHit appends a "throttle/login-throttle/auth-fail" event to
// the per-IP sliding window. ONLY suspicious events feed this — normal,
// successful traffic does not. This mirrors god's frontend_guard.go ban
// semantics: a token-bucket exhaustion or an auth-shaped 401/403 counts
// as a "naughty hit", and an IP that produces >BanThreshold of these in
// BanWindow gets a BanDuration ban.
//
// Returns (banned, until). Caller already holds g.mu.
func (g *rewireGuard) recordNaughtyHit(ip string) (banned bool, until time.Time) {
	b := g.all[ip]
	if b == nil {
		b = &bucket{}
		g.all[ip] = b
	}
	now := time.Now()
	if !b.banUntil.IsZero() && now.Before(b.banUntil) {
		return true, b.banUntil
	}
	cutoff := now.Add(-g.cfg.BanWindow)
	keep := b.hits[:0]
	for _, t := range b.hits {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	b.hits = append(keep, now)
	if len(b.hits) >= g.cfg.BanThreshold {
		b.banUntil = now.Add(g.cfg.BanDuration)
		b.hits = nil
		return true, b.banUntil
	}
	return false, time.Time{}
}

func (g *rewireGuard) isLoginPath(p string) bool {
	return strings.HasPrefix(p, "/api/login") ||
		strings.HasPrefix(p, "/api/admin/") ||
		strings.HasPrefix(p, "/api/buy/dodo-webhook") ||
		strings.HasPrefix(p, "/api/buy/rzp-webhook")
}

func (g *rewireGuard) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Admin guard endpoints are auth-gated themselves and MUST stay
		// reachable even from a banned IP - otherwise the operator
		// who triggers a self-ban has no recovery channel.
		if strings.HasPrefix(r.URL.Path, "/api/admin/guard/") {
			next.ServeHTTP(w, r)
			return
		}
		if g.disabled.Load() {
			next.ServeHTTP(w, r)
			return
		}
		ip := rewireClientIP(r)
		if g.isExempt(ip) {
			next.ServeHTTP(w, r)
			return
		}
		g.mu.Lock()
		// First: if this IP is already banned, kick immediately and
		// record nothing further (don't let the ban window grow under
		// sustained pressure - the ban already covers the window).
		if b := g.all[ip]; b != nil && !b.banUntil.IsZero() && time.Now().Before(b.banUntil) {
			until := b.banUntil
			g.mu.Unlock()
			g.banned.Add(1)
			retry := int(time.Until(until).Seconds()) + 1
			if retry < 1 {
				retry = 60
			}
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			http.Error(w, fmt.Sprintf("banned for %ds", retry), http.StatusTooManyRequests)
			return
		}
		// Login bucket (tighter) on auth-shaped endpoints.
		isLogin := g.isLoginPath(r.URL.Path)
		if isLogin {
			if !g.take(g.login, ip, g.cfg.LoginRPSBurst, g.cfg.LoginRPSRefill) {
				// Login bucket exhaustion counts as a naughty hit.
				banned, until := g.recordNaughtyHit(ip)
				g.mu.Unlock()
				g.dropped.Add(1)
				if banned {
					g.banned.Add(1)
					retry := int(time.Until(until).Seconds()) + 1
					if retry < 1 {
						retry = 60
					}
					w.Header().Set("Retry-After", strconv.Itoa(retry))
					g.emit("ban", ip, r.UserAgent(), r.URL.Path, r.Method,
						"login_bucket_repeated",
						fmt.Sprintf("ban_until=%s (triggered by login throttle)", until.UTC().Format(time.RFC3339)),
						"alert", http.StatusTooManyRequests)
					http.Error(w, fmt.Sprintf("banned for %ds", retry), http.StatusTooManyRequests)
					return
				}
				g.emit("login_throttle", ip, r.UserAgent(), r.URL.Path, r.Method,
					"rate_exceeded", "login bucket exhausted", "alert", http.StatusTooManyRequests)
				w.Header().Set("Retry-After", "30")
				http.Error(w, "rate limit", http.StatusTooManyRequests)
				return
			}
		}
		// Main token bucket. Bucket exhaustion -> naughty hit.
		if !g.take(g.all, ip, g.cfg.RPSBurst, g.cfg.RPSRefill) {
			banned, until := g.recordNaughtyHit(ip)
			g.mu.Unlock()
			g.dropped.Add(1)
			if banned {
				g.banned.Add(1)
				retry := int(time.Until(until).Seconds()) + 1
				if retry < 1 {
					retry = 60
				}
				w.Header().Set("Retry-After", strconv.Itoa(retry))
				g.emit("ban", ip, r.UserAgent(), r.URL.Path, r.Method,
					"main_bucket_repeated",
					fmt.Sprintf("ban_until=%s (triggered by main throttle)", until.UTC().Format(time.RFC3339)),
					"alert", http.StatusTooManyRequests)
				http.Error(w, fmt.Sprintf("banned for %ds", retry), http.StatusTooManyRequests)
				return
			}
			g.emit("throttle", ip, r.UserAgent(), r.URL.Path, r.Method,
				"rate_exceeded", "main bucket exhausted", "warn", http.StatusTooManyRequests)
			w.Header().Set("Retry-After", "5")
			http.Error(w, "rate limit", http.StatusTooManyRequests)
			return
		}
		g.mu.Unlock()
		g.allowed.Add(1)
		// On login/admin paths, also count an upstream 401/403 as a
		// naughty hit (brute-force-shaped failure). Wrap the response
		// writer to observe the status without buffering the body.
		if isLogin {
			ww := &rewireStatusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(ww, r)
			if ww.status == http.StatusUnauthorized || ww.status == http.StatusForbidden {
				g.mu.Lock()
				banned, until := g.recordNaughtyHit(ip)
				g.mu.Unlock()
				if banned {
					g.banned.Add(1)
					g.emit("ban", ip, r.UserAgent(), r.URL.Path, r.Method,
						"auth_fail_repeated",
						fmt.Sprintf("ban_until=%s (triggered by repeated %d)", until.UTC().Format(time.RFC3339), ww.status),
						"alert", http.StatusForbidden)
				} else {
					g.emit("auth_fail", ip, r.UserAgent(), r.URL.Path, r.Method,
						fmt.Sprintf("upstream_%d", ww.status),
						"upstream returned auth failure", "warn", ww.status)
				}
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

// rewireStatusWriter captures status code without buffering body so we can
// classify auth failures from downstream handlers.
type rewireStatusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *rewireStatusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *rewireStatusWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.status = http.StatusOK
		w.wroteHeader = true
	}
	return w.ResponseWriter.Write(b)
}

// Flush proxies the underlying ResponseWriter's Flush so streaming endpoints
// (audio, SSE) still work through the middleware.
func (w *rewireStatusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (g *rewireGuard) handleStats(w http.ResponseWriter, r *http.Request) {
	if !g.authAdmin(w, r) {
		return
	}
	g.mu.Lock()
	g.refreshBannedCount()
	bans := []map[string]any{}
	now := time.Now()
	for ip, b := range g.all {
		if !b.banUntil.IsZero() && now.Before(b.banUntil) {
			bans = append(bans, map[string]any{
				"ip":         ip,
				"until":      b.banUntil.UTC().Format(time.RFC3339),
				"seconds_left": int(time.Until(b.banUntil).Seconds()),
			})
		}
	}
	bucketsTracked := len(g.all)
	g.mu.Unlock()
	writeRewireJSON(w, 200, map[string]any{
		"disabled":         g.disabled.Load(),
		"allowed":          g.allowed.Load(),
		"throttled":        g.dropped.Load(),
		"banned_events":    g.banned.Load(),
		"banned_now":       g.bannedNow.Load(),
		"buckets_tracked":  bucketsTracked,
		"bans":             bans,
		"config": map[string]any{
			"rps_refill":     g.cfg.RPSRefill,
			"rps_burst":      g.cfg.RPSBurst,
			"login_refill":   g.cfg.LoginRPSRefill,
			"login_burst":    g.cfg.LoginRPSBurst,
			"ban_threshold":  g.cfg.BanThreshold,
			"ban_window_sec": int(g.cfg.BanWindow.Seconds()),
			"ban_duration_sec": int(g.cfg.BanDuration.Seconds()),
		},
	})
}

func (g *rewireGuard) handleDisable(w http.ResponseWriter, r *http.Request) {
	if !g.authAdmin(w, r) {
		return
	}
	g.disabled.Store(true)
	g.emit("admin_action", rewireClientIP(r), r.UserAgent(), r.URL.Path, r.Method,
		"guard_disabled", "via /api/admin/guard/disable", "warn", 200)
	writeRewireJSON(w, 200, map[string]any{"disabled": true})
}

func (g *rewireGuard) handleEnable(w http.ResponseWriter, r *http.Request) {
	if !g.authAdmin(w, r) {
		return
	}
	g.disabled.Store(false)
	g.emit("admin_action", rewireClientIP(r), r.UserAgent(), r.URL.Path, r.Method,
		"guard_enabled", "via /api/admin/guard/enable", "info", 200)
	writeRewireJSON(w, 200, map[string]any{"disabled": false})
}

func (g *rewireGuard) handleUnban(w http.ResponseWriter, r *http.Request) {
	if !g.authAdmin(w, r) {
		return
	}
	ip := strings.TrimSpace(r.URL.Query().Get("ip"))
	if ip == "" {
		http.Error(w, "ip required", 400)
		return
	}
	g.mu.Lock()
	if b, ok := g.all[ip]; ok {
		b.banUntil = time.Time{}
		b.hits = nil
	}
	g.mu.Unlock()
	g.emit("admin_action", rewireClientIP(r), r.UserAgent(), r.URL.Path, r.Method,
		"unban", "target="+ip, "info", 200)
	writeRewireJSON(w, 200, map[string]any{"unbanned": ip})
}

func (g *rewireGuard) handleClear(w http.ResponseWriter, r *http.Request) {
	if !g.authAdmin(w, r) {
		return
	}
	g.mu.Lock()
	g.all = make(map[string]*bucket)
	g.login = make(map[string]*bucket)
	g.mu.Unlock()
	g.emit("admin_action", rewireClientIP(r), r.UserAgent(), r.URL.Path, r.Method,
		"clear_all", "buckets+bans reset", "warn", 200)
	writeRewireJSON(w, 200, map[string]any{"cleared": true})
}

func (g *rewireGuard) authAdmin(w http.ResponseWriter, r *http.Request) bool {
	if g.cfg.AdminToken == "" {
		http.Error(w, "guard admin disabled (REWIRE_ADMIN_TOKEN unset)", 503)
		return false
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		http.Error(w, "missing bearer", 401)
		return false
	}
	if strings.TrimPrefix(auth, "Bearer ") != g.cfg.AdminToken {
		http.Error(w, "bad token", 403)
		return false
	}
	return true
}

func writeRewireJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func insecureClientTransport() http.RoundTripper {
	// Loopback target (god backend on 127.0.0.1:9871) uses a real cert
	// for gogillu.live - we accept any cert here because we're talking
	// to localhost over loopback only.
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	return t
}
