// Package main — Dodo Payments integration (v1.7.0).
//
// Why: Razorpay denied activation for the Rewire merchant account
// (UPI couldn't be enabled, and even card-tile checkout was rejecting
// with "No appropriate payment method found"). User got approval on
// Dodo Payments instead. This file is the Dodo equivalent of buy_rzp.go
// and is intended to be the PRIMARY payment path in production while
// the rzp_* routes are kept for rollback only.
//
// Flow (hosted Checkout Sessions):
//   1. POST /api/buy/init           → creates local order (existing).
//   2. POST /api/buy/dodo-checkout  → creates a Dodo checkout session via
//                                      REST, binds session id to the local
//                                      order, returns { checkout_url }.
//   3. Browser navigates to checkout_url (Dodo's hosted page). User pays.
//   4. Dodo redirects browser back to
// Sample success URL (Dodo will redirect here):
//      https://rewire.gogillu.live/buy?dodo_order=<local_order_id>.
//      Frontend polls /api/buy/status until status='approved'.
//   5. POST /api/buy/dodo-webhook    → Standard-Webhooks-signed event from
//                                      Dodo. On payment.succeeded we mint
//                                      the lifetime token + mark approved.
//
// Env vars:
//   REWIRE_DODO_API_KEY         (Bearer token, live or test)
//   REWIRE_DODO_WEBHOOK_SECRET  (whsec_*, Standard Webhooks)
//   REWIRE_DODO_PRODUCT_ID      (pdt_*)
//   REWIRE_DODO_ENV             ("live" or "test"; defaults to "live")
//
// If any of API_KEY / PRODUCT_ID is absent, the dodo routes return 503
// (mirrors buy_rzp.go's behaviour).

package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// ---------- config helpers ----------

func dodoAPIKey() string         { return strings.TrimSpace(os.Getenv("REWIRE_DODO_API_KEY")) }
func dodoWebhookSecret() string  { return strings.TrimSpace(os.Getenv("REWIRE_DODO_WEBHOOK_SECRET")) }
func dodoProductID() string      { return strings.TrimSpace(os.Getenv("REWIRE_DODO_PRODUCT_ID")) }
func dodoEnv() string {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("REWIRE_DODO_ENV")))
	if v == "" {
		return "live"
	}
	return v
}
// dodoEnabled requires the API key, product id, AND the webhook secret —
// because without the secret the webhook handler is a no-op, which would
// silently lose every payment.succeeded event.
func dodoEnabled() bool {
	return dodoAPIKey() != "" && dodoProductID() != "" && dodoWebhookSecret() != ""
}

// dodoAPIBase returns the host for the Dodo Payments REST API.
// Per https://docs.dodopayments.com/api-reference/introduction:
//   test → https://test.dodopayments.com
//   live → https://live.dodopayments.com
func dodoAPIBase() string {
	if dodoEnv() == "test" {
		return "https://test.dodopayments.com"
	}
	return "https://live.dodopayments.com"
}

// dodoReturnURL is where Dodo redirects the user after payment completes
// (success OR failure). Frontend reads ?dodo_order=<local> + ?status=
// and polls /api/buy/status until approved.
func dodoReturnURL(localOrderID string) string {
	base := strings.TrimSpace(os.Getenv("REWIRE_DODO_RETURN_BASE"))
	if base == "" {
		base = "https://rewire.gogillu.live/buy"
	}
	base = strings.TrimRight(base, "/?")
	return base + "?dodo_order=" + localOrderID
}

// ---------- POST /api/buy/dodo-checkout ----------

type dodoCheckoutReqBody struct {
	OrderID string `json:"order_id"`
	Contact string `json:"contact,omitempty"`
}

type dodoCheckoutResp struct {
	CheckoutURL string `json:"checkout_url"`
	SessionID   string `json:"session_id"`
	Amount      int    `json:"amount"`   // paise
	Currency    string `json:"currency"` // "INR"
}

func (s *Server) handleBuyDodoCheckout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !dodoEnabled() {
		http.Error(w, "dodo payments not configured on this server", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4*1024)
	var b dodoCheckoutReqBody
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

	var email, status, anonID, existingSession string
	err := s.db.QueryRowContext(ctx, `
        SELECT email, status, COALESCE(anon_id,''), COALESCE(dodo_session_id,'')
          FROM payment_orders WHERE order_id = ? AND expires_at > ?
    `, b.OrderID, now).Scan(&email, &status, &anonID, &existingSession)
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

	// Build the create-checkout-session request. Documented at:
	// https://docs.dodopayments.com/api-reference/checkout-sessions/create
	//
	// We pin the product to the env-configured product_id (₹9 lifetime),
	// pre-fill the customer email, force the billing currency to INR,
	// and embed the local order id + email in metadata so the webhook
	// can resolve our row without needing a DB lookup by Dodo IDs.
	custName := emailLocalPart(email)
	body := map[string]any{
		"product_cart": []map[string]any{{
			"product_id": dodoProductID(),
			"quantity":   1,
		}},
		"customer": map[string]any{
			"email": email,
			"name":  custName,
		},
		"billing_currency": "INR",
		"return_url":       dodoReturnURL(b.OrderID),
		"metadata": map[string]string{
			"local_order_id": b.OrderID,
			"email":          email,
			"product":        "Rewire Premium Lifetime",
		},
		// Customer should NOT be able to edit their email — the token
		// will be mailed to whatever the buy page collected.
		"feature_flags": map[string]any{
			"allow_customer_editing_email": false,
			"allow_phone_number_collection": false,
		},
		// Keep checkout minimal — we don't need a full billing address
		// for a ₹9 digital lifetime token; Dodo will collect just what
		// the chosen payment method requires.
		"minimal_address": true,
	}
	bodyBytes, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST",
		dodoAPIBase()+"/checkouts", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+dodoAPIKey())

	httpClient := &http.Client{Timeout: 12 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		http.Error(w, "dodo unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 32*1024))
	if resp.StatusCode/100 != 2 {
		fmt.Fprintf(os.Stderr, "rewire: dodo checkout reject %d: %s\n",
			resp.StatusCode, string(respBody))
		http.Error(w, "dodo rejected checkout: "+string(respBody), http.StatusBadGateway)
		return
	}
	var parsed struct {
		SessionID   string `json:"session_id"`
		CheckoutURL string `json:"checkout_url"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil || parsed.CheckoutURL == "" {
		http.Error(w, "dodo returned invalid session: "+string(respBody), http.StatusBadGateway)
		return
	}

	// Persist session id alongside the local order so we can correlate
	// the webhook later.
	if _, err := s.db.ExecContext(ctx, `
        UPDATE payment_orders SET dodo_session_id=?, updated_at=? WHERE order_id=?
    `, parsed.SessionID, now, b.OrderID); err != nil {
		fmt.Fprintf(os.Stderr, "rewire: dodo session persist failed: %v\n", err)
	}

	writeJSON(w, http.StatusOK, dodoCheckoutResp{
		CheckoutURL: parsed.CheckoutURL,
		SessionID:   parsed.SessionID,
		Amount:      premiumPricePaise,
		Currency:    "INR",
	})
}

// emailLocalPart returns the substring before "@" — used to seed
// Dodo's "customer name" field when we only have an email on file.
func emailLocalPart(email string) string {
	i := strings.IndexByte(email, '@')
	if i <= 0 {
		return "Rewire user"
	}
	name := email[:i]
	if name == "" {
		return "Rewire user"
	}
	return name
}

// ---------- POST /api/buy/dodo-webhook ----------
//
// Standard Webhooks (https://www.standardwebhooks.com/) signed events.
// Headers:
//   webhook-id        — unique event id, use for idempotency.
//   webhook-timestamp — unix seconds, must be within 5 min skew.
//   webhook-signature — "v1,<base64>" (possibly multiple space-separated).
//
// Signing key:
//   secret_bytes = base64_decode(strip_prefix("whsec_"))
//
// Signed message = id + "." + ts + "." + body, HMAC-SHA256 with secret_bytes.

func (s *Server) handleBuyDodoWebhook(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	secret := dodoWebhookSecret()
	if secret == "" {
		// No secret configured → webhook is disabled. Reply 200 so Dodo
		// stops retrying.
		w.WriteHeader(http.StatusOK)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256*1024)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read fail", http.StatusBadRequest)
		return
	}
	msgID := strings.TrimSpace(r.Header.Get("webhook-id"))
	msgTS := strings.TrimSpace(r.Header.Get("webhook-timestamp"))
	msgSig := strings.TrimSpace(r.Header.Get("webhook-signature"))
	if msgID == "" || msgTS == "" || msgSig == "" {
		http.Error(w, "missing webhook headers", http.StatusBadRequest)
		return
	}

	// 5-minute timestamp tolerance to defeat replay attacks. Dodo retries
	// with exponential backoff, but the spec says to reject anything
	// older than 5 min so the rejected event won't be re-sent forever.
	tsInt, err := strconv.ParseInt(msgTS, 10, 64)
	if err != nil {
		http.Error(w, "bad timestamp", http.StatusBadRequest)
		return
	}
	if delta := time.Now().Unix() - tsInt; delta > 300 || delta < -300 {
		http.Error(w, "timestamp outside tolerance", http.StatusBadRequest)
		return
	}

	if !verifyStandardWebhookSig(secret, msgID, msgTS, body, msgSig) {
		http.Error(w, "bad signature", http.StatusBadRequest)
		return
	}

	var ev struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Data      struct {
			PayloadType  string            `json:"payload_type"`
			PaymentID    string            `json:"payment_id"`
			Status       string            `json:"status"`
			TotalAmount  int               `json:"total_amount"`
			Currency     string            `json:"currency"`
			CheckoutSess string            `json:"checkout_session_id"`
			Metadata     map[string]string `json:"metadata"`
			ProductCart  []struct {
				ProductID string `json:"product_id"`
				Quantity  int    `json:"quantity"`
			} `json:"product_cart"`
			Customer struct {
				Email string `json:"email"`
				Name  string `json:"name"`
			} `json:"customer"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}

	// We only act on payment.succeeded. Other events (payment.failed,
	// subscription.*, refund.*, dispute.*, license_key.*) are accepted
	// with 200 so Dodo doesn't retry, but they're no-ops here.
	if ev.Type != "payment.succeeded" {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Defence-in-depth: validate the event actually corresponds to a
	// Rewire Premium purchase BEFORE we trust local_order_id.
	//   * status must be "succeeded" (not "processing" / "failed").
	//   * currency, if reported, must be INR (we never accept non-INR
	//     for Rewire Premium).
	//   * total_amount, if reported >0, must be >= premiumPricePaise
	//     (Dodo's adaptive currency or partial-amount flows shouldn't
	//     apply here — we created the session with a fixed product).
	//   * product_cart, if reported, must include our configured
	//     product id. Catches misconfigured business / wrong-webhook.
	if ev.Data.Status != "" && ev.Data.Status != "succeeded" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if ev.Data.Currency != "" && ev.Data.Currency != "INR" {
		fmt.Fprintf(os.Stderr,
			"rewire: dodo webhook payment.succeeded rejected — currency=%s, want INR (payment_id=%s)\n",
			ev.Data.Currency, ev.Data.PaymentID)
		w.WriteHeader(http.StatusOK)
		return
	}
	if ev.Data.TotalAmount > 0 && ev.Data.TotalAmount < premiumPricePaise {
		fmt.Fprintf(os.Stderr,
			"rewire: dodo webhook payment.succeeded rejected — amount=%d, want >=%d (payment_id=%s)\n",
			ev.Data.TotalAmount, premiumPricePaise, ev.Data.PaymentID)
		w.WriteHeader(http.StatusOK)
		return
	}
	if len(ev.Data.ProductCart) > 0 {
		want := dodoProductID()
		found := false
		for _, p := range ev.Data.ProductCart {
			if p.ProductID == want {
				found = true
				break
			}
		}
		if !found {
			fmt.Fprintf(os.Stderr,
				"rewire: dodo webhook payment.succeeded rejected — product_cart missing %s (payment_id=%s)\n",
				want, ev.Data.PaymentID)
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	localID := strings.TrimSpace(ev.Data.Metadata["local_order_id"])
	if localID == "" {
		// Fall back to looking up by session id.
		_ = s.db.QueryRowContext(r.Context(),
			`SELECT order_id FROM payment_orders WHERE dodo_session_id = ? LIMIT 1`,
			ev.Data.CheckoutSess).Scan(&localID)
	}
	if localID == "" {
		// Best-effort: nothing we can do, accept the event so Dodo
		// stops retrying.
		fmt.Fprintf(os.Stderr,
			"rewire: dodo webhook payment.succeeded with no resolvable local_order_id (payment_id=%s)\n",
			ev.Data.PaymentID)
		w.WriteHeader(http.StatusOK)
		return
	}

	now := time.Now().UnixMilli()
	if err := s.dodoApproveAndMint(r.Context(), msgID, localID, ev.Data.PaymentID, ev.Data.CheckoutSess, now); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// verifyStandardWebhookSig implements the Standard Webhooks verification:
//   1. Secret may be either raw bytes or `whsec_<base64>` — both supported.
//   2. signed_payload = id + "." + ts + "." + body
//   3. mac = HMAC-SHA256(secret_bytes, signed_payload), base64-encoded.
//   4. The header is "v1,<base64>" — possibly several space-separated.
func verifyStandardWebhookSig(secret, msgID, msgTS string, body []byte, sigHeader string) bool {
	// Resolve the actual signing key.
	keyBytes := []byte(secret)
	if strings.HasPrefix(secret, "whsec_") {
		if decoded, err := base64.StdEncoding.DecodeString(secret[len("whsec_"):]); err == nil && len(decoded) > 0 {
			keyBytes = decoded
		}
	}
	mac := hmac.New(sha256.New, keyBytes)
	mac.Write([]byte(msgID))
	mac.Write([]byte("."))
	mac.Write([]byte(msgTS))
	mac.Write([]byte("."))
	mac.Write(body)
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	// Header may contain multiple signatures, e.g. "v1,abc v1,def". Walk
	// each one and compare in constant time.
	expectedB := []byte(expected)
	for _, tok := range strings.Fields(sigHeader) {
		// Each token is "vN,<base64>". We accept v1 only.
		parts := strings.SplitN(tok, ",", 2)
		if len(parts) != 2 || parts[0] != "v1" {
			continue
		}
		if hmac.Equal([]byte(parts[1]), expectedB) {
			return true
		}
	}
	return false
}

// dodoApproveAndMint is the post-webhook idempotent approval path. It
// mirrors handleBuyRzpVerify's "BEGIN IMMEDIATE → check status → mint
// token → update row → enqueue email → commit" flow, minus the
// signature step (which the webhook handler did upstream).
//
// Idempotency is BAKED INTO THE TRANSACTION: we INSERT OR IGNORE on
// dodo_webhook_seen INSIDE the tx, and if it's a duplicate we COMMIT
// (with no other writes) and return. This eliminates the race where
// the idempotency marker is written before the approval succeeds.
func (s *Server) dodoApproveAndMint(ctx context.Context, webhookID, localID, dodoPaymentID, dodoSessionID string, now int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, _ = tx.ExecContext(ctx, `BEGIN IMMEDIATE`)

	// Idempotency inside the tx. If the marker insert is a no-op,
	// this webhook id has already been processed by a successful
	// earlier delivery — commit (no-op) and return.
	res, err := tx.ExecContext(ctx, `
        INSERT OR IGNORE INTO dodo_webhook_seen (webhook_id, received_at) VALUES (?, ?)
    `, webhookID, now)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return tx.Commit()
	}

	var email, status string
	if err := tx.QueryRowContext(ctx,
		`SELECT email, status FROM payment_orders WHERE order_id = ?`, localID).Scan(&email, &status); err != nil {
		return fmt.Errorf("dodo approve: %w", err)
	}
	if status == "approved" {
		// Local order already approved by a different code path (e.g.
		// the rzp flow). Idempotent success — keep the webhook-seen
		// marker.
		return tx.Commit()
	}

	raw, hash, err := s.mintToken()
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO premium_tokens (token_hash, email, order_id, issued_at)
        VALUES (?, ?, ?, ?)
    `, hash, email, localID, now); err != nil {
		return err
	}
	utrKey := "DODO-" + dodoPaymentID
	if _, err := tx.ExecContext(ctx, `
        INSERT OR REPLACE INTO issued_tokens (order_id, utr, raw_token, created_at)
        VALUES (?, ?, ?, ?)
    `, localID, utrKey, raw, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
        UPDATE payment_orders
        SET status='approved',
            utr=?,
            token_hash=?,
            dodo_session_id=COALESCE(NULLIF(?,''), dodo_session_id),
            dodo_payment_id=?,
            reviewer_note=COALESCE(reviewer_note,'') || ' [auto-verified by dodo webhook]',
            updated_at=?,
            approved_at=?
        WHERE order_id=?
    `, utrKey, hash, dodoSessionID, dodoPaymentID, now, now, localID); err != nil {
		return err
	}

	subject := "Your Rewire Premium token (lifetime) ✨"
	html := fmt.Sprintf(
		`<div style="font-family:system-ui,sans-serif;max-width:540px;margin:0 auto;padding:24px">
		   <h2 style="margin:0 0 12px">Welcome to Rewire Premium ✨</h2>
		   <p>Order: <code>%s</code><br>Dodo payment: <code>%s</code></p>
		   <p style="margin-top:18px"><a href="https://rewire.gogillu.live/premium?t=%s" style="display:inline-block;background:linear-gradient(90deg,#ff007a,#ff8a00);color:#fff;padding:12px 22px;border-radius:999px;font-weight:700;text-decoration:none">Tap here to unlock Premium →</a></p>
		   <p style="margin-top:18px">Or paste this <b>lifetime token</b> on <a href="https://rewire.gogillu.live/premium">rewire.gogillu.live/premium</a>:</p>
		   <pre style="background:#111;color:#fff;padding:14px;border-radius:8px;overflow:auto;font-size:13px">%s</pre>
		   <p style="opacity:.7;font-size:12px">Disputes / lost tokens: reply to this email or write to admin@gogillu.live.</p>
		 </div>`,
		localID, dodoPaymentID, raw, raw)
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO email_outbox (kind, order_id, to_addr, subject, html, next_attempt_at, created_at)
        VALUES ('token', ?, ?, ?, ?, ?, ?)
        ON CONFLICT(kind, order_id) WHERE order_id IS NOT NULL DO UPDATE SET
            to_addr=excluded.to_addr, subject=excluded.subject, html=excluded.html,
            status='pending', attempts=0, next_attempt_at=excluded.next_attempt_at
    `, localID, email, subject, html, now, now); err != nil {
		return err
	}

	return tx.Commit()
}

// ---------- POST /api/buy/dodo-complete ----------
//
// After the user returns from Dodo's hosted checkout and the webhook has
// flipped status to 'approved', the frontend calls this endpoint to
// retrieve the freshly-minted lifetime token. The session id (returned
// from /api/buy/dodo-checkout and persisted in localStorage) acts as the
// recovery code — symmetric with /api/buy/complete's order_id+utr pair.
// Without it, a polling attacker who knew only the order_id couldn't
// retrieve someone else's token.
//
// One-time delivery: the raw token row is dropped after the first
// successful return, matching /api/buy/complete's behaviour.

type dodoCompleteBody struct {
	OrderID   string `json:"order_id"`
	SessionID string `json:"session_id"`
}

func (s *Server) handleBuyDodoComplete(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, 4*1024)
	var b dodoCompleteBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	b.OrderID = strings.TrimSpace(b.OrderID)
	b.SessionID = strings.TrimSpace(b.SessionID)
	if b.OrderID == "" || b.SessionID == "" {
		http.Error(w, "order_id+session_id required", http.StatusBadRequest)
		return
	}

	var status, boundSession, dodoPaymentID string
	err := s.db.QueryRowContext(r.Context(), `
        SELECT status, COALESCE(dodo_session_id,''), COALESCE(dodo_payment_id,'')
          FROM payment_orders WHERE order_id = ?
    `, b.OrderID).Scan(&status, &boundSession, &dodoPaymentID)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if boundSession == "" || boundSession != b.SessionID {
		http.Error(w, "session_id mismatch", http.StatusBadRequest)
		return
	}
	if status != "approved" {
		writeJSON(w, http.StatusOK, map[string]any{"status": status, "token": ""})
		return
	}

	utrKey := "DODO-" + dodoPaymentID
	var raw string
	_ = s.db.QueryRowContext(r.Context(),
		`SELECT COALESCE(raw_token,'') FROM issued_tokens WHERE order_id = ? AND utr = ?`,
		b.OrderID, utrKey).Scan(&raw)
	if raw == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "approved", "token": "",
			"note": "token already retrieved; check email",
		})
		return
	}
	// One-time delivery — drop the raw row.
	_, _ = s.db.ExecContext(r.Context(),
		`DELETE FROM issued_tokens WHERE order_id = ? AND utr = ?`, b.OrderID, utrKey)
	writeJSON(w, http.StatusOK, map[string]any{"status": "approved", "token": raw})
}

// migrateDodoColumns idempotently adds the dodo_* columns to payment_orders
// and creates the dodo_webhook_seen idempotency table. Must be invoked once
// at startup from main.go.
func (s *Server) migrateDodoColumns() {
	stmts := []string{
		`ALTER TABLE payment_orders ADD COLUMN dodo_session_id TEXT`,
		`ALTER TABLE payment_orders ADD COLUMN dodo_payment_id TEXT`,
		`CREATE TABLE IF NOT EXISTS dodo_webhook_seen (
            webhook_id  TEXT PRIMARY KEY,
            received_at INTEGER NOT NULL
        )`,
	}
	for _, q := range stmts {
		_, err := s.db.Exec(q)
		if err != nil && !strings.Contains(err.Error(), "duplicate column") {
			_ = err
		}
	}
}
