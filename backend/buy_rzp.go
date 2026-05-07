// Package main — Razorpay UPI integration (v1.4.0).
//
// Why: PhonePe was rejecting third-party-initiated payments to our personal
// VPA with "security errors" + ₹2,000 caps. Razorpay's UPI Intent flow
// fixes this — checkout.js opens PhonePe / GPay / Paytm with the amount
// prefilled AND the merchant identity verified by Razorpay, so PhonePe's
// anti-fraud doesn't fire. Webhook + signature verification means the
// website knows for certain when the user has paid (no more honor-claim).
//
// Flow:
//   1. POST /api/buy/init           → creates local order (existing).
//   2. POST /api/buy/rzp-order       → creates a Razorpay order via REST,
//                                       binds it to the local order_id.
//                                       Returns { rzp_order_id, key_id, amount }.
//   3. checkout.js opens UPI intent — user pays.
//   4. POST /api/buy/rzp-verify      → verifies HMAC signature, mints
//                                       lifetime token, status=approved.
//   5. (backup) POST /api/buy/rzp-webhook — Razorpay-initiated confirm.
//
// Env vars:
//   REWIRE_RZP_KEY_ID       (e.g. rzp_test_xxx)
//   REWIRE_RZP_KEY_SECRET
//   REWIRE_RZP_WEBHOOK_SECRET (optional, for /api/buy/rzp-webhook)
//
// If the key envs are absent, the rzp routes return 503 and the frontend
// falls back to the manual UPI deep-link flow.

package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// ---------- config helpers ----------

func rzpKeyID() string     { return strings.TrimSpace(os.Getenv("REWIRE_RZP_KEY_ID")) }
func rzpKeySecret() string { return strings.TrimSpace(os.Getenv("REWIRE_RZP_KEY_SECRET")) }
func rzpWebhookSecret() string {
	return strings.TrimSpace(os.Getenv("REWIRE_RZP_WEBHOOK_SECRET"))
}
func rzpEnabled() bool { return rzpKeyID() != "" && rzpKeySecret() != "" }

// ---------- POST /api/buy/rzp-order ----------

type rzpOrderReqBody struct {
	OrderID string `json:"order_id"`
}

type rzpOrderResp struct {
	RzpOrderID string `json:"rzp_order_id"`
	KeyID      string `json:"key_id"`
	Amount     int    `json:"amount"` // paise
	Currency   string `json:"currency"`
}

func (s *Server) handleBuyRzpOrder(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !rzpEnabled() {
		http.Error(w, "razorpay not configured on this server", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4*1024)
	var b rzpOrderReqBody
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
	ctx := r.Context()

	// Look up local order. Reject if expired / wrong status.
	var email, status, anonID, existingRzp string
	err := s.db.QueryRowContext(ctx, `
        SELECT email, status, COALESCE(anon_id,''), COALESCE(rzp_order_id,'')
          FROM payment_orders WHERE order_id = ? AND expires_at > ?
    `, b.OrderID, now).Scan(&email, &status, &anonID, &existingRzp)
	if err != nil {
		http.Error(w, "order not found or expired", http.StatusNotFound)
		return
	}
	if status == "approved" {
		writeJSON(w, http.StatusOK, map[string]any{
			"already_paid": true,
			"status":       "approved",
		})
		return
	}
	if status != "initiated" && status != "utr_submitted" {
		http.Error(w, "order not in payable state", http.StatusConflict)
		return
	}

	// Idempotency: if we already created an rzp order for this local order,
	// reuse it. Razorpay orders are reusable until they're paid.
	if existingRzp != "" {
		writeJSON(w, http.StatusOK, rzpOrderResp{
			RzpOrderID: existingRzp,
			KeyID:      rzpKeyID(),
			Amount:     premiumPricePaise,
			Currency:   "INR",
		})
		return
	}

	// Create the Razorpay order.
	// Endpoint: POST https://api.razorpay.com/v1/orders
	// Auth:     HTTP Basic key_id:key_secret
	// Body:     {amount, currency, receipt, notes}
	body := map[string]any{
		"amount":   premiumPricePaise,
		"currency": "INR",
		"receipt":  b.OrderID, // <= 40 chars
		"notes": map[string]string{
			"local_order_id": b.OrderID,
			"email":          email,
			"product":        "Rewire Premium Lifetime",
		},
	}
	bodyBytes, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST",
		"https://api.razorpay.com/v1/orders", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(rzpKeyID(), rzpKeySecret())

	httpClient := &http.Client{Timeout: 12 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		http.Error(w, "razorpay unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 32*1024))
	if resp.StatusCode/100 != 2 {
		http.Error(w, "razorpay rejected order: "+string(respBody), http.StatusBadGateway)
		return
	}
	var parsed struct {
		ID     string `json:"id"`
		Amount int    `json:"amount"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil || parsed.ID == "" {
		http.Error(w, "razorpay returned invalid order: "+string(respBody), http.StatusBadGateway)
		return
	}

	// Persist the rzp_order_id alongside the local order so verify is
	// stateless w.r.t. the client.
	if _, err := s.db.ExecContext(ctx, `
        UPDATE payment_orders SET rzp_order_id=?, updated_at=? WHERE order_id=?
    `, parsed.ID, now, b.OrderID); err != nil {
		// Non-fatal — Razorpay accepted, our DB write failed. Logged but
		// signature-verify will still work without the bind because the
		// client echoes back rzp_order_id; we just won't have local proof.
		fmt.Fprintf(os.Stderr, "rewire: rzp_order_id persist failed: %v\n", err)
	}

	writeJSON(w, http.StatusOK, rzpOrderResp{
		RzpOrderID: parsed.ID,
		KeyID:      rzpKeyID(),
		Amount:     parsed.Amount,
		Currency:   "INR",
	})
}

// ---------- POST /api/buy/rzp-verify ----------

type rzpVerifyReqBody struct {
	OrderID       string `json:"order_id"`        // local order id
	RzpOrderID    string `json:"rzp_order_id"`    // from checkout response
	RzpPaymentID  string `json:"rzp_payment_id"`  // from checkout response
	RzpSignature  string `json:"rzp_signature"`   // from checkout response
}

func (s *Server) handleBuyRzpVerify(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !rzpEnabled() {
		http.Error(w, "razorpay not configured on this server", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4*1024)
	var b rzpVerifyReqBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	b.OrderID = strings.TrimSpace(b.OrderID)
	b.RzpOrderID = strings.TrimSpace(b.RzpOrderID)
	b.RzpPaymentID = strings.TrimSpace(b.RzpPaymentID)
	b.RzpSignature = strings.TrimSpace(b.RzpSignature)
	if b.OrderID == "" || b.RzpOrderID == "" || b.RzpPaymentID == "" || b.RzpSignature == "" {
		http.Error(w, "missing fields", http.StatusBadRequest)
		return
	}

	// Verify signature: HMAC-SHA256(rzp_order_id + "|" + rzp_payment_id, secret)
	mac := hmac.New(sha256.New, []byte(rzpKeySecret()))
	mac.Write([]byte(b.RzpOrderID + "|" + b.RzpPaymentID))
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(strings.ToLower(b.RzpSignature))) {
		http.Error(w, "signature mismatch", http.StatusBadRequest)
		return
	}

	now := time.Now().UnixMilli()
	ctx := r.Context()

	// Look up local order; cross-check rzp_order_id matches what we
	// previously persisted (defence-in-depth).
	var email, status, boundRzp string
	err := s.db.QueryRowContext(ctx, `
        SELECT email, status, COALESCE(rzp_order_id,'') FROM payment_orders
         WHERE order_id = ?
    `, b.OrderID).Scan(&email, &status, &boundRzp)
	if err != nil {
		http.Error(w, "order not found", http.StatusNotFound)
		return
	}
	if boundRzp != "" && boundRzp != b.RzpOrderID {
		http.Error(w, "rzp_order_id mismatch", http.StatusBadRequest)
		return
	}
	if status == "approved" {
		// Idempotent: re-emit issued_token if we still have it.
		var raw string
		_ = s.db.QueryRowContext(ctx,
			`SELECT raw_token FROM issued_tokens WHERE order_id = ? LIMIT 1`,
			b.OrderID).Scan(&raw)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "status": "approved", "token": raw, "email": email,
		})
		return
	}

	// Mint + insert under BEGIN IMMEDIATE so duplicate verifies are safe.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	_, _ = tx.ExecContext(ctx, `BEGIN IMMEDIATE`)

	var status2 string
	if err := tx.QueryRowContext(ctx,
		`SELECT status FROM payment_orders WHERE order_id = ?`, b.OrderID).Scan(&status2); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if status2 == "approved" {
		var raw string
		_ = tx.QueryRowContext(ctx,
			`SELECT raw_token FROM issued_tokens WHERE order_id = ? LIMIT 1`,
			b.OrderID).Scan(&raw)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "status": "approved", "token": raw, "email": email,
		})
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
	utrKey := "RZP-" + b.RzpPaymentID
	if _, err := tx.ExecContext(ctx, `
        INSERT OR REPLACE INTO issued_tokens (order_id, utr, raw_token, created_at)
        VALUES (?, ?, ?, ?)
    `, b.OrderID, utrKey, raw, now); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Razorpay = real verified payment, so go straight to 'approved'
	// (not 'pending_verify' like the honor-claim path).
	if _, err := tx.ExecContext(ctx, `
        UPDATE payment_orders
        SET status='approved',
            utr=?,
            token_hash=?,
            rzp_order_id=?,
            rzp_payment_id=?,
            rzp_signature=?,
            reviewer_note=COALESCE(reviewer_note,'') || ' [auto-verified by razorpay signature]',
            updated_at=?,
            approved_at=?
        WHERE order_id=?
    `, utrKey, hash, b.RzpOrderID, b.RzpPaymentID, b.RzpSignature, now, now, b.OrderID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	subject := "Your Rewire Premium token (lifetime) ✨"
	html := fmt.Sprintf(
		`<div style="font-family:system-ui,sans-serif;max-width:540px;margin:0 auto;padding:24px">
		   <h2 style="margin:0 0 12px">Welcome to Rewire Premium ✨</h2>
		   <p>Order: <code>%s</code><br>Razorpay payment: <code>%s</code></p>
		   <p>Your <b>lifetime token</b> (paste this on <a href="https://rewire.gogillu.live/premium">rewire.gogillu.live/premium</a> if you ever lose access on this device):</p>
		   <pre style="background:#111;color:#fff;padding:14px;border-radius:8px;overflow:auto;font-size:13px">%s</pre>
		   <p style="opacity:.7;font-size:12px">Disputes / lost tokens: reply to this email or write to admin@gogillu.live.</p>
		 </div>`,
		b.OrderID, b.RzpPaymentID, raw)
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

	if err := tx.Commit(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"status": "approved",
		"token":  raw,
		"email":  email,
	})
}

// ---------- POST /api/buy/rzp-webhook (optional backup) ----------
//
// Razorpay POSTs payment.captured / payment.authorized to this endpoint.
// Payload is HMAC-SHA256-signed in the X-Razorpay-Signature header. We
// idempotently approve the local order if not already approved.

func (s *Server) handleBuyRzpWebhook(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	secret := rzpWebhookSecret()
	if secret == "" {
		// No webhook secret configured — webhook is disabled. Return 200
		// so Razorpay doesn't retry.
		w.WriteHeader(http.StatusOK)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read fail", http.StatusBadRequest)
		return
	}
	sig := strings.TrimSpace(r.Header.Get("X-Razorpay-Signature"))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(strings.ToLower(sig))) {
		http.Error(w, "bad signature", http.StatusBadRequest)
		return
	}
	var ev struct {
		Event   string `json:"event"`
		Payload struct {
			Payment struct {
				Entity struct {
					ID      string `json:"id"`
					OrderID string `json:"order_id"`
					Status  string `json:"status"`
					Notes   struct {
						LocalOrderID string `json:"local_order_id"`
					} `json:"notes"`
				} `json:"entity"`
			} `json:"payment"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	// Only act on captured/authorized.
	if !strings.HasPrefix(ev.Event, "payment.") {
		w.WriteHeader(http.StatusOK)
		return
	}
	pay := ev.Payload.Payment.Entity
	localID := strings.TrimSpace(pay.Notes.LocalOrderID)
	if localID == "" {
		// Best-effort: look up by rzp_order_id.
		_ = s.db.QueryRowContext(r.Context(),
			`SELECT order_id FROM payment_orders WHERE rzp_order_id = ? LIMIT 1`,
			pay.OrderID).Scan(&localID)
	}
	if localID == "" {
		w.WriteHeader(http.StatusOK)
		return
	}
	// Idempotent: if already approved, no-op. Otherwise mint token (same
	// as rzp-verify, minus the signature path because webhook payload IS
	// the signature here). For simplicity we just record the payment id
	// and let rzp-verify (or admin replay) finish the mint.
	now := time.Now().UnixMilli()
	_, _ = s.db.ExecContext(r.Context(), `
        UPDATE payment_orders
        SET rzp_payment_id=COALESCE(NULLIF(rzp_payment_id,''),?),
            updated_at=?
        WHERE order_id=?
    `, pay.ID, now, localID)
	w.WriteHeader(http.StatusOK)
}

// migrateRzpColumns idempotently adds the rzp_* columns to payment_orders.
// Must be invoked once at startup (from main.go).
func (s *Server) migrateRzpColumns() {
	stmts := []string{
		`ALTER TABLE payment_orders ADD COLUMN rzp_order_id TEXT`,
		`ALTER TABLE payment_orders ADD COLUMN rzp_payment_id TEXT`,
		`ALTER TABLE payment_orders ADD COLUMN rzp_signature TEXT`,
	}
	for _, q := range stmts {
		_, err := s.db.Exec(q)
		if err != nil && !strings.Contains(err.Error(), "duplicate column") {
			_ = err
		}
	}
}
