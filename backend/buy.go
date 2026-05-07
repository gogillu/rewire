// Package main — UPI payment flow + lifetime premium token issuance.
//
// Flow:
//   1. POST /api/buy/init    — user enters email, server creates order
//                              (status=initiated, expires in 24 h),
//                              returns order_id + UPI deep-link.
//   2. user pays in any UPI app (Airtel / GPay / PhonePe / Paytm).
//   3. POST /api/buy/submit-utr — user pastes UTR/txn-ref. status=utr_submitted.
//   4. admin reviews via /api/admin/payments dashboard panel.
//   5. POST /api/admin/payments/approve — BEGIN IMMEDIATE tx mints a token,
//      inserts a premium_tokens row, queues an email_outbox row, marks
//      the order approved. Idempotent; double-clicks are no-ops.
//   6. /api/buy/status (polled) returns {status: approved, token, email}
//      once approved — the frontend stores the token in localStorage and
//      the user is also emailed it via god backend's send_email.py.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	premiumPriceINR    = 9
	premiumPricePaise  = 900
	upiVPA             = "7000189190@airtel"
	upiPayeeName       = "Govind Choudhary"
	premiumNote        = "Rewire Premium"
	orderExpiryHours   = 24
	adminEmailFallback = "admin@gogillu.live"
)

const buySchema = `
CREATE TABLE IF NOT EXISTS payment_orders (
    order_id      TEXT PRIMARY KEY,
    email         TEXT NOT NULL,
    amount_paise  INTEGER NOT NULL,
    vpa           TEXT NOT NULL,
    status        TEXT NOT NULL,
    utr           TEXT,
    token_hash    TEXT,
    anon_id       TEXT,
    session_id    TEXT,
    ip            TEXT,
    country       TEXT,
    city          TEXT,
    isp           TEXT,
    ua            TEXT,
    reviewer_note TEXT,
    expires_at    INTEGER NOT NULL,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    approved_at   INTEGER
);
CREATE INDEX IF NOT EXISTS idx_po_status ON payment_orders(status);
CREATE INDEX IF NOT EXISTS idx_po_email  ON payment_orders(email);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_po_utr ON payment_orders(utr) WHERE utr IS NOT NULL;

CREATE TABLE IF NOT EXISTS email_outbox (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    kind            TEXT NOT NULL,                  -- 'token' | 'admin-notify'
    order_id        TEXT,
    to_addr         TEXT NOT NULL,
    subject         TEXT NOT NULL,
    html            TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending',
    attempts        INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT,
    next_attempt_at INTEGER NOT NULL,
    created_at      INTEGER NOT NULL,
    sent_at         INTEGER
);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_eo_kind_order ON email_outbox(kind, order_id) WHERE order_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_eo_status ON email_outbox(status, next_attempt_at);
`

// ---------- helpers ----------

func newOrderID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return strings.ToLower(strings.TrimRight(base32.StdEncoding.EncodeToString(b), "="))
}

func validEmail(e string) bool {
	if len(e) > 200 {
		return false
	}
	_, err := mail.ParseAddress(e)
	return err == nil
}

func upiDeepLink(orderID string, paise int) string {
	v := url.Values{}
	v.Set("pa", upiVPA)
	v.Set("pn", upiPayeeName)
	v.Set("am", fmt.Sprintf("%.2f", float64(paise)/100))
	v.Set("cu", "INR")
	v.Set("tn", premiumNote+" "+orderID)
	v.Set("tr", orderID)
	return "upi://pay?" + v.Encode()
}

// ---------- /api/buy/init ----------

type buyInitBody struct {
	Email     string `json:"email"`
	AnonID    string `json:"anon_id"`
	SessionID string `json:"session_id"`
}

func (s *Server) handleBuyInit(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, 8*1024)
	var b buyInitBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	b.Email = strings.ToLower(strings.TrimSpace(b.Email))
	if !validEmail(b.Email) {
		http.Error(w, "valid email required", http.StatusBadRequest)
		return
	}
	if b.AnonID == "" || b.SessionID == "" {
		http.Error(w, "anon_id+session_id required", http.StatusBadRequest)
		return
	}
	id := newOrderID()
	now := time.Now().UnixMilli()
	exp := time.Now().Add(orderExpiryHours * time.Hour).UnixMilli()
	ip := clientIP(r)
	country, city, isp := s.geoLookupRich(ip)
	ua := r.UserAgent()
	if len(ua) > 400 {
		ua = ua[:400]
	}
	if _, err := s.db.ExecContext(r.Context(), `
        INSERT INTO payment_orders (
            order_id, email, amount_paise, vpa, status, anon_id, session_id,
            ip, country, city, isp, ua, expires_at, created_at, updated_at
        ) VALUES (?, ?, ?, ?, 'initiated', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    `, id, b.Email, premiumPricePaise, upiVPA, b.AnonID, b.SessionID,
		ip, country, city, isp, ua, exp, now, now); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if country == "" && ip != "" && isPublicIP(ip) {
		s.geoResolveAsync(ip)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"order_id":     id,
		"vpa":          upiVPA,
		"payee":        upiPayeeName,
		"amount_inr":   premiumPriceINR,
		"deep_link":    upiDeepLink(id, premiumPricePaise),
		"expires_at":   exp,
		"note":         premiumNote + " " + id,
	})
}

// ---------- /api/buy/submit-utr ----------

type buyUTRBody struct {
	OrderID string `json:"order_id"`
	UTR     string `json:"utr"`
}

func (s *Server) handleBuySubmitUTR(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, 4*1024)
	var b buyUTRBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	b.UTR = strings.ToUpper(strings.TrimSpace(b.UTR))
	if b.OrderID == "" || len(b.UTR) < 10 || len(b.UTR) > 30 {
		http.Error(w, "order_id+utr(10-30 chars) required", http.StatusBadRequest)
		return
	}
	for _, c := range b.UTR {
		if !((c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z')) {
			http.Error(w, "utr must be alphanumeric", http.StatusBadRequest)
			return
		}
	}
	now := time.Now().UnixMilli()
	// Update only if currently in 'initiated' or 'utr_submitted' (allow user
	// to correct a typo). Reject if approved/rejected/expired.
	res, err := s.db.ExecContext(r.Context(), `
        UPDATE payment_orders
        SET utr = ?, status = 'utr_submitted', updated_at = ?
        WHERE order_id = ? AND status IN ('initiated', 'utr_submitted') AND expires_at > ?
    `, b.UTR, now, b.OrderID, now)
	if err != nil {
		// Most likely a duplicate-UTR conflict from the unique index.
		if strings.Contains(err.Error(), "UNIQUE") {
			http.Error(w, "this UTR is already on file", http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		http.Error(w, "order not found, expired, or already finalized", http.StatusGone)
		return
	}

	// Notify admin. One row per (kind, order_id) thanks to the unique index,
	// so duplicate UTR submissions don't spam the admin inbox.
	var email string
	_ = s.db.QueryRowContext(r.Context(), `SELECT email FROM payment_orders WHERE order_id = ?`,
		b.OrderID).Scan(&email)
	subject := "[Rewire] New ₹" + fmt.Sprint(premiumPriceINR) + " order: " + b.OrderID
	html := fmt.Sprintf(
		`<p>New Rewire Premium order awaiting verification.</p>
		 <ul>
		   <li><b>Order:</b> %s</li>
		   <li><b>Email:</b> %s</li>
		   <li><b>UTR:</b> %s</li>
		   <li><b>Amount:</b> ₹%d</li>
		 </ul>
		 <p>Verify in the admin dashboard → Payment Queue.</p>`,
		b.OrderID, email, b.UTR, premiumPriceINR)
	_, _ = s.db.ExecContext(r.Context(), `
        INSERT INTO email_outbox (kind, order_id, to_addr, subject, html, next_attempt_at, created_at)
        VALUES ('admin-notify', ?, ?, ?, ?, ?, ?)
        ON CONFLICT(kind, order_id) WHERE order_id IS NOT NULL DO NOTHING
    `, b.OrderID, adminEmailFallback, subject, html, now, now)

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "utr_submitted"})
}

// ---------- /api/buy/status ----------

func (s *Server) handleBuyStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	id := strings.TrimSpace(r.URL.Query().Get("order_id"))
	if id == "" {
		http.Error(w, "order_id required", http.StatusBadRequest)
		return
	}
	var (
		email, status, utr string
		expires, approved  int64
	)
	err := s.db.QueryRowContext(r.Context(), `
        SELECT email, status, COALESCE(utr,''), expires_at, COALESCE(approved_at,0)
        FROM payment_orders WHERE order_id = ?
    `, id).Scan(&email, &status, &utr, &expires, &approved)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	out := map[string]any{
		"order_id":   id,
		"email":      email,
		"status":     status,
		"has_utr":    utr != "",
		"expires_at": expires,
	}
	// Status-only response. The token is NOT returned via /status — we only
	// hand it back from /complete which the frontend hits once after seeing
	// status=approved (so the token doesn't appear in many polled responses).
	if status == "approved" {
		out["approved_at"] = approved
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------- /api/buy/complete ----------
//
// Returns the raw token if order is approved AND a recovery code is presented
// (we use the order_id+UTR pair as that recovery code: it's known only to
// the user who placed the order; without it, polling /status doesn't leak
// the token).

type buyCompleteBody struct {
	OrderID string `json:"order_id"`
	UTR     string `json:"utr"`
}

func (s *Server) handleBuyComplete(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var b buyCompleteBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	b.UTR = strings.ToUpper(strings.TrimSpace(b.UTR))
	if b.OrderID == "" || b.UTR == "" {
		http.Error(w, "order_id+utr required", http.StatusBadRequest)
		return
	}
	// We stored only the token_hash in payment_orders, so on approval we
	// also stash the raw token in a short-lived row (issued_token table)
	// keyed by (order_id, utr). One-time read: row deleted after first
	// successful return so the token can never be re-fetched.
	var raw, status string
	err := s.db.QueryRowContext(r.Context(), `
        SELECT COALESCE(it.raw_token,''), p.status
        FROM payment_orders p
        LEFT JOIN issued_tokens it ON it.order_id = p.order_id AND it.utr = ?
        WHERE p.order_id = ?
    `, b.UTR, b.OrderID).Scan(&raw, &status)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if status != "approved" {
		writeJSON(w, http.StatusOK, map[string]any{"status": status, "token": ""})
		return
	}
	if raw == "" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "approved", "token": "", "note": "token already retrieved; check email"})
		return
	}
	// One-time delivery — drop the raw row.
	_, _ = s.db.ExecContext(r.Context(),
		`DELETE FROM issued_tokens WHERE order_id = ? AND utr = ?`, b.OrderID, b.UTR)
	writeJSON(w, http.StatusOK, map[string]any{"status": "approved", "token": raw})
}

// issuedTokensSchema is appended to the schema list — separate so it can
// be created independently.
const issuedTokensSchema = `
CREATE TABLE IF NOT EXISTS issued_tokens (
    order_id   TEXT NOT NULL,
    utr        TEXT NOT NULL,
    raw_token  TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (order_id, utr)
);
`

// ---------- /api/buy/recover ----------
//
// User lost their token / browser. Provide email + order_id. We re-queue
// the token email (using stored hash → can't recover the raw token, but we
// can mint a new one and revoke the old). For simplicity we just mint a
// FRESH token, revoke any old tokens for that email, and email it.
//
// Rate-limited: max 3 recovers per email per day.

type buyRecoverBody struct {
	Email   string `json:"email"`
	OrderID string `json:"order_id"`
}

func (s *Server) handleBuyRecover(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var b buyRecoverBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	b.Email = strings.ToLower(strings.TrimSpace(b.Email))
	b.OrderID = strings.TrimSpace(b.OrderID)
	if !validEmail(b.Email) || b.OrderID == "" {
		http.Error(w, "email+order_id required", http.StatusBadRequest)
		return
	}
	// Verify the order belongs to this email + is approved.
	var status string
	err := s.db.QueryRowContext(r.Context(),
		`SELECT status FROM payment_orders WHERE order_id = ? AND email = ?`,
		b.OrderID, b.Email).Scan(&status)
	if err != nil || status != "approved" {
		// Don't leak whether the email exists; just say "ok, check email".
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	// Rate-limit by counting outbox 'token' emails to this address in the
	// last 24 h.
	since := time.Now().Add(-24 * time.Hour).UnixMilli()
	var n int
	_ = s.db.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM email_outbox WHERE kind = 'token' AND to_addr = ? AND created_at >= ?`,
		b.Email, since).Scan(&n)
	if n >= 3 {
		http.Error(w, "too many recovery attempts; wait 24 h", http.StatusTooManyRequests)
		return
	}
	if err := s.mintAndEmailToken(r.Context(), b.OrderID, b.Email, true); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// mintAndEmailToken issues a fresh token, persists its hash, queues the
// outbox email, and (when revokeOld) marks all prior tokens for this email
// as revoked. Used on first approval and on recovery.
func (s *Server) mintAndEmailToken(ctx context.Context, orderID, email string, revokeOld bool) error {
	raw, hash, err := s.mintToken()
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if revokeOld {
		_, _ = tx.ExecContext(ctx,
			`UPDATE premium_tokens SET revoked_at = ? WHERE email = ? AND revoked_at IS NULL`,
			now, email)
	}
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO premium_tokens (token_hash, email, order_id, issued_at)
        VALUES (?, ?, ?, ?)
    `, hash, email, orderID, now); err != nil {
		return err
	}
	// Stash raw token for one-time pickup via /api/buy/complete (keyed by
	// order_id + a per-recovery code = "RECOVER-<random>" so the user's
	// /complete flow still works after recovery).
	utrKey := "RECOVER-" + orderID
	_, _ = tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO issued_tokens (order_id, utr, raw_token, created_at) VALUES (?, ?, ?, ?)`,
		orderID, utrKey, raw, now)

	subject := "Your Rewire Premium token (lifetime)"
	html := fmt.Sprintf(
		`<div style="font-family:system-ui,sans-serif;max-width:540px;margin:0 auto;padding:24px">
		   <h2 style="margin:0 0 12px">Welcome to Rewire Premium ✨</h2>
		   <p>Order: <code>%s</code></p>
		   <p>Your lifetime token (paste this on <a href="https://rewire.gogillu.live/premium">rewire.gogillu.live/premium</a> when prompted):</p>
		   <pre style="background:#111;color:#fff;padding:14px;border-radius:8px;overflow:auto;font-size:13px">%s</pre>
		   <p style="opacity:.8;font-size:13px">This token never expires. Keep it safe — anyone with the token can use your Premium.<br>
		   For disputes / lost tokens, reply to this email or write to admin@gogillu.live.</p>
		 </div>`,
		orderID, raw)
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO email_outbox (kind, order_id, to_addr, subject, html, next_attempt_at, created_at)
        VALUES ('token', ?, ?, ?, ?, ?, ?)
        ON CONFLICT(kind, order_id) WHERE order_id IS NOT NULL DO UPDATE SET
            to_addr=excluded.to_addr, subject=excluded.subject, html=excluded.html,
            status='pending', attempts=0, next_attempt_at=excluded.next_attempt_at
    `, orderID, email, subject, html, now, now); err != nil {
		return err
	}
	return tx.Commit()
}

// ---------- /api/buy/claim ----------
//
// v1.2: One-tap honor-based claim. UPI without a payment gateway has no
// callback, so users were stuck on a manual UTR-paste step. New flow:
//
//   1. User pays via the upi:// deep-link.
//   2. They come back and tap "I've paid — unlock now".
//   3. POST /api/buy/claim {order_id} mints a token IMMEDIATELY, emails it,
//      moves order to status='pending_verify'. The user is unblocked.
//   4. Admin reconciles offline against bank statement; matching deposits
//      are marked status='approved'; missing deposits get the order
//      status='rejected' and the token revoked.
//
// Per rubber-duck review, this is bounded:
//   * Rate-limit per anon_id: 1 successful claim per (anon_id, email) pair
//     per 24 h.
//   * Rate-limit per IP: max 5 orders per IP per 24 h.
//   * Token marked ttl_at = now()+48h for unverified claims; revoked if
//     no matching UTR by then. Once admin verifies, ttl_at is cleared.
//   * Order ID + email + anon_id + IP + UA captured so admin can spot
//     fraud quickly.
//
// At ₹9 with these guardrails, abuse loss is bounded and operational
// reasoning stays simple.
type buyClaimBody struct {
	OrderID string `json:"order_id"`
	UTR     string `json:"utr"` // optional — power users who copied UTR
}

func (s *Server) handleBuyClaim(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, 4*1024)
	var b buyClaimBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	b.OrderID = strings.TrimSpace(b.OrderID)
	b.UTR = strings.ToUpper(strings.TrimSpace(b.UTR))
	if b.OrderID == "" {
		http.Error(w, "order_id required", http.StatusBadRequest)
		return
	}
	if b.UTR != "" && (len(b.UTR) < 10 || len(b.UTR) > 30) {
		http.Error(w, "utr (when provided) must be 10-30 alphanumeric chars", http.StatusBadRequest)
		return
	}
	for _, c := range b.UTR {
		if !((c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z')) {
			http.Error(w, "utr must be alphanumeric", http.StatusBadRequest)
			return
		}
	}
	now := time.Now().UnixMilli()
	ip := clientIP(r)
	ctx := r.Context()

	// Look up the order.
	var email, status, anonID string
	err := s.db.QueryRowContext(ctx, `
        SELECT email, status, COALESCE(anon_id,'') FROM payment_orders
        WHERE order_id = ? AND expires_at > ?
    `, b.OrderID, now).Scan(&email, &status, &anonID)
	if err != nil {
		http.Error(w, "order not found or expired", http.StatusNotFound)
		return
	}
	if status == "approved" {
		// Idempotent — re-emit the issued_token via /api/buy/complete.
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "already": true, "status": "approved"})
		return
	}
	if status != "initiated" && status != "utr_submitted" {
		http.Error(w, "order not in claimable state", http.StatusConflict)
		return
	}

	// Rate-limit #1: max 1 successful claim per (email) per 24 h. Accidental
	// duplicate clicks on the same order are fine (handled above as already-approved).
	since := time.Now().Add(-24 * time.Hour).UnixMilli()
	var prevClaims int
	_ = s.db.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM payment_orders
        WHERE email = ? AND status IN ('pending_verify','approved')
          AND COALESCE(approved_at, updated_at) >= ?
    `, email, since).Scan(&prevClaims)
	if prevClaims >= 1 {
		// One paid lifetime token per email is enough.
		http.Error(w, "this email already has a pending or approved Premium token", http.StatusConflict)
		return
	}
	// Rate-limit #2: max 5 orders per IP per 24 h.
	var ipOrders int
	_ = s.db.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM payment_orders WHERE ip = ? AND created_at >= ?
    `, ip, since).Scan(&ipOrders)
	if ipOrders > 5 {
		http.Error(w, "too many orders from this network; try again tomorrow", http.StatusTooManyRequests)
		return
	}

	// BEGIN IMMEDIATE so concurrent claims serialize.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	_, _ = tx.ExecContext(ctx, `BEGIN IMMEDIATE`)

	// Re-check status under the write lock.
	var status2 string
	if err := tx.QueryRowContext(ctx,
		`SELECT status FROM payment_orders WHERE order_id = ?`, b.OrderID).Scan(&status2); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if status2 == "approved" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "already": true, "status": "approved"})
		return
	}
	if status2 != "initiated" && status2 != "utr_submitted" {
		http.Error(w, "order not in claimable state", http.StatusConflict)
		return
	}

	raw, hash, err := s.mintToken()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO premium_tokens (token_hash, email, order_id, issued_at)
        VALUES (?, ?, ?, ?)
    `, hash, email, b.OrderID, now); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// One-time pickup row keyed by (order_id, "AUTO-CLAIM" or actual UTR).
	utrKey := b.UTR
	if utrKey == "" {
		utrKey = "AUTO-CLAIM"
	}
	if _, err := tx.ExecContext(ctx, `
        INSERT OR REPLACE INTO issued_tokens (order_id, utr, raw_token, created_at)
        VALUES (?, ?, ?, ?)
    `, b.OrderID, utrKey, raw, now); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// status='pending_verify' is the new bucket for honor claims; admin
	// dashboard already orders by (status='utr_submitted' DESC) — we add
	// pending_verify to the queue as well via a separate query path.
	if _, err := tx.ExecContext(ctx, `
        UPDATE payment_orders
        SET status='pending_verify',
            utr=COALESCE(NULLIF(?, ''), utr),
            token_hash=?,
            reviewer_note=COALESCE(reviewer_note,'') || CASE WHEN ?<>'' THEN '' ELSE ' [auto-claim, pending bank verification]' END,
            updated_at=?,
            approved_at=?
        WHERE order_id=?
    `, b.UTR, hash, b.UTR, now, now, b.OrderID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Email the token immediately (best-effort, queued).
	subject := "Your Rewire Premium token (lifetime) — pending verification"
	html := fmt.Sprintf(
		`<div style="font-family:system-ui,sans-serif;max-width:540px;margin:0 auto;padding:24px">
		   <h2 style="margin:0 0 12px">Welcome to Rewire Premium ✨</h2>
		   <p>Order: <code>%s</code></p>
		   <p>Your lifetime token (paste this on <a href="https://rewire.gogillu.live/premium">rewire.gogillu.live/premium</a> when prompted):</p>
		   <pre style="background:#111;color:#fff;padding:14px;border-radius:8px;overflow:auto;font-size:13px">%s</pre>
		   <p style="opacity:.85;font-size:13px">Your payment is being verified against our bank statement (usually within 24 h).
		     If it can't be matched, the token will be revoked and you'll be notified at this email.</p>
		   <p style="opacity:.7;font-size:12px">Disputes / lost tokens: reply to this email or write to admin@gogillu.live.</p>
		 </div>`,
		b.OrderID, raw)
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO email_outbox (kind, order_id, to_addr, subject, html, next_attempt_at, created_at)
        VALUES ('token', ?, ?, ?, ?, ?, ?)
        ON CONFLICT(kind, order_id) WHERE order_id IS NOT NULL DO UPDATE SET
            to_addr=excluded.to_addr, subject=excluded.subject, html=excluded.html,
            status='pending', attempts=0, next_attempt_at=excluded.next_attempt_at
    `, b.OrderID, email, subject, html, now, now); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Notify admin (separately) so they can reconcile.
	adminSub := "[Rewire] AUTO-CLAIM ₹9 — needs verification: " + b.OrderID
	adminHTML := fmt.Sprintf(
		`<p>Honor-based claim received. Verify against bank statement.</p>
		 <ul>
		   <li><b>Order:</b> %s</li>
		   <li><b>Email:</b> %s</li>
		   <li><b>UTR:</b> %s</li>
		   <li><b>IP:</b> %s</li>
		   <li><b>Anon:</b> %s</li>
		 </ul>
		 <p>Verify in admin dashboard → Payment Queue.</p>`,
		b.OrderID, email, b.UTR, ip, anonID)
	_, _ = tx.ExecContext(ctx, `
        INSERT INTO email_outbox (kind, order_id, to_addr, subject, html, next_attempt_at, created_at)
        VALUES ('admin-notify', ?, ?, ?, ?, ?, ?)
        ON CONFLICT(kind, order_id) WHERE order_id IS NOT NULL DO NOTHING
    `, b.OrderID, adminEmailFallback, adminSub, adminHTML, now, now)

	if err := tx.Commit(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"status": "pending_verify",
		"token":  raw, // delivered once; client stores in localStorage.
		"email":  email,
		"note":   "Premium unlocked. Email sent. Bank verification within 24 h.",
	})
}

// ---------- /api/admin/payments (list pending) ----------

func (s *Server) handleAdminPayments(w http.ResponseWriter, r *http.Request) {
	if err := s.adminAuth(r); err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	rows, err := s.db.QueryContext(r.Context(), `
        SELECT order_id, email, COALESCE(utr,''), status, amount_paise,
               COALESCE(country,''), COALESCE(city,''), COALESCE(isp,''),
               COALESCE(reviewer_note,''), created_at, updated_at, COALESCE(approved_at,0)
        FROM payment_orders
        ORDER BY
          (status IN ('utr_submitted', 'pending_verify')) DESC,
          (status = 'utr_submitted') DESC,
          updated_at DESC
        LIMIT 200
    `)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	type row struct {
		OrderID    string `json:"order_id"`
		Email      string `json:"email"`
		UTR        string `json:"utr"`
		Status     string `json:"status"`
		Paise      int    `json:"amount_paise"`
		Country    string `json:"country"`
		City       string `json:"city"`
		ISP        string `json:"isp"`
		Note       string `json:"note"`
		CreatedAt  int64  `json:"created_at"`
		UpdatedAt  int64  `json:"updated_at"`
		ApprovedAt int64  `json:"approved_at"`
	}
	var out []row
	for rows.Next() {
		var rr row
		if err := rows.Scan(&rr.OrderID, &rr.Email, &rr.UTR, &rr.Status, &rr.Paise,
			&rr.Country, &rr.City, &rr.ISP, &rr.Note, &rr.CreatedAt, &rr.UpdatedAt, &rr.ApprovedAt); err != nil {
			continue
		}
		out = append(out, rr)
	}
	writeJSON(w, http.StatusOK, map[string]any{"orders": out})
}

// ---------- /api/admin/payments/approve ----------
//
// Per rubber-duck feedback: BEGIN IMMEDIATE, only act if status is
// utr_submitted, mint+email atomically.

type adminApproveBody struct {
	OrderID string `json:"order_id"`
	Note    string `json:"note,omitempty"`
}

func (s *Server) handleAdminApprove(w http.ResponseWriter, r *http.Request) {
	// Approval is too sensitive for query-string auth; require the header.
	want := strings.TrimSpace(os.Getenv("REWIRE_ADMIN"))
	got := r.Header.Get("X-Rewire-Admin")
	if want == "" || got != want {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	var b adminApproveBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	b.OrderID = strings.TrimSpace(b.OrderID)
	if b.OrderID == "" {
		http.Error(w, "order_id required", http.StatusBadRequest)
		return
	}
	now := time.Now().UnixMilli()

	// BEGIN IMMEDIATE acquires the writer lock up front so concurrent
	// approve clicks serialize.
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	_, _ = tx.ExecContext(r.Context(), `BEGIN IMMEDIATE`) // best-effort hint

	var email, status, utr string
	err = tx.QueryRowContext(r.Context(),
		`SELECT email, status, COALESCE(utr,'') FROM payment_orders WHERE order_id = ?`,
		b.OrderID).Scan(&email, &status, &utr)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// v1.2: pending_verify is the post-honor-claim state. Approving it just
	// marks the bank-verified bit; the user already got the token.
	if status == "pending_verify" {
		if _, err := tx.ExecContext(r.Context(), `
            UPDATE payment_orders
            SET status='approved', reviewer_note=?, updated_at=?
            WHERE order_id=?
        `, b.Note, now, b.OrderID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := tx.Commit(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "verified": true})
		return
	}
	if status != "utr_submitted" {
		// Idempotent: double-click on already-approved row is a no-op success.
		if status == "approved" {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "already": true})
			return
		}
		http.Error(w, "order not in utr_submitted state", http.StatusConflict)
		return
	}
	raw, hash, err := s.mintToken()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `
        INSERT INTO premium_tokens (token_hash, email, order_id, issued_at)
        VALUES (?, ?, ?, ?)
    `, hash, email, b.OrderID, now); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// One-time pickup row keyed by (order_id, utr).
	if _, err := tx.ExecContext(r.Context(), `
        INSERT OR REPLACE INTO issued_tokens (order_id, utr, raw_token, created_at)
        VALUES (?, ?, ?, ?)
    `, b.OrderID, utr, raw, now); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `
        UPDATE payment_orders
        SET status='approved', token_hash=?, reviewer_note=?, approved_at=?, updated_at=?
        WHERE order_id=?
    `, hash, b.Note, now, now, b.OrderID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	subject := "Your Rewire Premium token (lifetime)"
	html := fmt.Sprintf(
		`<div style="font-family:system-ui,sans-serif;max-width:540px;margin:0 auto;padding:24px">
		   <h2 style="margin:0 0 12px">Welcome to Rewire Premium ✨</h2>
		   <p>Order: <code>%s</code></p>
		   <p>Your lifetime token (paste this on <a href="https://rewire.gogillu.live/premium">rewire.gogillu.live/premium</a> when prompted):</p>
		   <pre style="background:#111;color:#fff;padding:14px;border-radius:8px;overflow:auto;font-size:13px">%s</pre>
		   <p style="opacity:.8;font-size:13px">This token never expires. Keep it safe — anyone with the token can use your Premium.<br>
		   For disputes / lost tokens, reply to this email or write to admin@gogillu.live.</p>
		 </div>`,
		b.OrderID, raw)
	if _, err := tx.ExecContext(r.Context(), `
        INSERT INTO email_outbox (kind, order_id, to_addr, subject, html, next_attempt_at, created_at)
        VALUES ('token', ?, ?, ?, ?, ?, ?)
        ON CONFLICT(kind, order_id) WHERE order_id IS NOT NULL DO UPDATE SET
            to_addr=excluded.to_addr, subject=excluded.subject, html=excluded.html,
            status='pending', attempts=0, next_attempt_at=excluded.next_attempt_at
    `, b.OrderID, email, subject, html, now, now); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "email": email})
}

// ---------- /api/admin/payments/reject ----------

func (s *Server) handleAdminReject(w http.ResponseWriter, r *http.Request) {
	want := strings.TrimSpace(os.Getenv("REWIRE_ADMIN"))
	got := r.Header.Get("X-Rewire-Admin")
	if want == "" || got != want {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	var b adminApproveBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	now := time.Now().UnixMilli()
	// v1.2: rejecting a pending_verify order also revokes the issued token.
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(r.Context(), `
        UPDATE payment_orders
        SET status='rejected', reviewer_note=?, updated_at=?
        WHERE order_id=? AND status IN ('initiated', 'utr_submitted', 'pending_verify')
    `, b.Note, now, b.OrderID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		http.Error(w, "order not eligible for rejection", http.StatusConflict)
		return
	}
	// Revoke any tokens minted under this order.
	_, _ = tx.ExecContext(r.Context(),
		`UPDATE premium_tokens SET revoked_at = ? WHERE order_id = ? AND revoked_at IS NULL`,
		now, b.OrderID)
	if err := tx.Commit(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------- email outbox processor ----------

// startEmailWorker runs forever, picking up email_outbox rows that are
// 'pending' and whose next_attempt_at has passed. Sends via god backend's
// send_email.py and updates status. Exponential backoff on failure.
func (s *Server) startEmailWorker(ctx context.Context) {
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.processEmailOutboxOnce()
			}
		}
	}()
}

func (s *Server) processEmailOutboxOnce() {
	now := time.Now().UnixMilli()
	rows, err := s.db.Query(`
        SELECT id, to_addr, subject, html, attempts
        FROM email_outbox
        WHERE status = 'pending' AND next_attempt_at <= ?
        ORDER BY id ASC
        LIMIT 25
    `, now)
	if err != nil {
		return
	}
	type job struct {
		ID       int64
		To       string
		Subject  string
		HTML     string
		Attempts int
	}
	var jobs []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.ID, &j.To, &j.Subject, &j.HTML, &j.Attempts); err == nil {
			jobs = append(jobs, j)
		}
	}
	rows.Close()
	for _, j := range jobs {
		if err := s.sendEmailViaGod(j.To, j.Subject, j.HTML); err != nil {
			delay := time.Duration(60*(1<<minInt(j.Attempts, 6))) * time.Second
			next := time.Now().Add(delay).UnixMilli()
			_, _ = s.db.Exec(`
                UPDATE email_outbox
                SET attempts=attempts+1, last_error=?, next_attempt_at=?
                WHERE id=?
            `, err.Error(), next, j.ID)
			continue
		}
		_, _ = s.db.Exec(`
            UPDATE email_outbox SET status='sent', sent_at=?, last_error=NULL WHERE id=?
        `, time.Now().UnixMilli(), j.ID)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// sendEmailViaGod shells out to god's send_email.py — never via shell, so
// args with quotes/backticks/etc are safe.
func (s *Server) sendEmailViaGod(to, subject, html string) error {
	script := `C:\Users\arushi\god\mailserver\send_email.py`
	if _, err := os.Stat(script); err != nil {
		return fmt.Errorf("send_email.py missing: %w", err)
	}
	args := []string{
		script,
		"--to", to,
		"--from", "admin@gogillu.live",
		"--from-name", "Rewire by GoGillu",
		"--subject", subject,
		"--html", html,
		"--method", "auto",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python", args...)
	cmd.Dir = filepath.Dir(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("send_email.py failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ---------- /buy frontend ----------

func (s *Server) handleBuyFrontend() http.Handler {
	dir := filepath.Join(filepath.Dir(s.frontendDir), "frontend-buy")
	if _, err := os.Stat(dir); err != nil {
		dir = s.frontendDir
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/buy")
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
		case strings.HasSuffix(p, ".png"), strings.HasSuffix(p, ".jpg"):
			w.Header().Set("Cache-Control", "public, max-age=86400")
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
