// Rewire /buy — UPI checkout flow with manual UTR verification.
(function () {
  'use strict';
  const $ = (s) => document.querySelector(s);

  const SESSION_ID = (() => {
    let s = sessionStorage.getItem('rw_sid');
    if (!s) { s = uuid(); sessionStorage.setItem('rw_sid', s); }
    return s;
  })();
  const ANON_ID = (() => {
    let a = localStorage.getItem('rw_aid');
    if (!a) { a = uuid(); localStorage.setItem('rw_aid', a); }
    return a;
  })();
  function uuid() {
    if (crypto && crypto.randomUUID) return crypto.randomUUID();
    return 'xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx'.replace(/[xy]/g, c => {
      const r = Math.random()*16|0, v = c==='x' ? r : (r&0x3|0x8);
      return v.toString(16);
    });
  }

  // ---------- Telemetry (funnel events) ----------
  const evQueue = [];
  function track(type, fields = {}) {
    evQueue.push({
      ts: Date.now(),
      type,
      movie_id: fields.movie_id,
      duration_ms: fields.duration_ms,
      extra: fields.extra,
    });
    if (evQueue.length >= 8) flushEvents();
  }
  async function flushEvents(useBeacon = false) {
    if (evQueue.length === 0) return;
    const batch = evQueue.splice(0, evQueue.length);
    const body = JSON.stringify({
      session_id: SESSION_ID, anon_id: ANON_ID,
      os: navigator.platform || '', browser: '', device: '',
      screen_w: window.innerWidth, screen_h: window.innerHeight,
      mode: 'buy',
      events: batch,
    });
    try {
      if (useBeacon && navigator.sendBeacon) {
        navigator.sendBeacon('/api/events', new Blob([body], { type: 'application/json' }));
      } else {
        await fetch('/api/events', {
          method: 'POST', headers: { 'Content-Type': 'application/json' },
          body, keepalive: true,
        });
      }
    } catch {}
  }
  setInterval(() => flushEvents(false), 5000);
  window.addEventListener('pagehide', () => { track('session_end'); flushEvents(true); });
  track('buy_page_open');

  // ---------- State + step machine ----------
  const STEPS = ['email', 'pay', 'utr', 'pending', 'done', 'recover'];
  function show(step) {
    STEPS.forEach(s => {
      const el = document.getElementById('step-' + s);
      if (el) el.classList.toggle('active', s === step);
    });
    window.scrollTo({ top: 0 });
  }
  let order = null;          // {order_id, deep_link, vpa, expires_at, ...}
  let utrSubmitted = false;
  let pollTimer = null;

  // ---------- Step 1 — email ----------
  // v1.7.1: dropped the "Confirm email" second field. The inline note now
  // tells the user the token will be emailed here post-payment, so a single
  // well-validated field is enough.
  $('#goPay').addEventListener('click', async () => {
    const e1 = $('#email').value.trim().toLowerCase();
    if (!/^[^@\s]+@[^@\s]+\.[^@\s]+$/.test(e1)) { alert('Please enter a valid email.'); return; }
    track('buy_email_entered');
    $('#goPay').disabled = true;
    $('#goPay').textContent = 'Setting up…';
    try {
      const r = await fetch('/api/buy/init', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ email: e1, anon_id: ANON_ID, session_id: SESSION_ID }),
      });
      if (!r.ok) throw new Error(await r.text());
      order = await r.json();
      order.email = e1; // remember for Razorpay prefill
      // v1.4.2: capture optional phone to skip Razorpay's contact step.
      // Razorpay expects 10-digit only; the +91 prefix is added by the widget itself.
      const phoneRaw = ($('#phone') && $('#phone').value || '').replace(/\D/g, '');
      if (/^[6-9]\d{9}$/.test(phoneRaw)) order.contact = phoneRaw;
      localStorage.setItem('rw_buy_order', JSON.stringify(order));
      $('#orderRef').textContent = order.order_id;
      $('#upiBtn').href = order.deep_link;
      $('#qrImg').src = '/api/buy/qr?order_id=' + encodeURIComponent(order.order_id);
      track('buy_order_created', { extra: { order_id: order.order_id } });
      show('pay');
    } catch (err) {
      alert('Could not start order: ' + (err.message || err));
    } finally {
      $('#goPay').disabled = false;
      $('#goPay').textContent = 'Continue to payment →';
    }
  });

  // ---------- Recovery link ----------
  $('#recoverLink').addEventListener('click', (e) => {
    e.preventDefault();
    track('buy_recover_open');
    show('recover');
  });
  $('#backFromRecover').addEventListener('click', (e) => { e.preventDefault(); show('email'); });
  $('#recSubmit').addEventListener('click', async () => {
    const email = $('#recEmail').value.trim().toLowerCase();
    const oid = $('#recOrder').value.trim();
    if (!/^[^@\s]+@/.test(email) || !oid) { alert('Email + order id required'); return; }
    $('#recSubmit').disabled = true;
    try {
      const r = await fetch('/api/buy/recover', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ email, order_id: oid }),
      });
      if (!r.ok) throw new Error(await r.text());
      track('buy_recover_submit');
      alert('If that order matches, a fresh token has been emailed to ' + email + '.');
    } catch (err) {
      alert('Recovery failed: ' + (err.message || err));
    } finally {
      $('#recSubmit').disabled = false;
    }
  });

  // ---------- Step 2 — pay ----------
  // v1.7.0: Dodo Payments primary (Razorpay merchant activation denied).
  //         Razorpay retained as secondary/legacy. Manual UPI deep-link
  //         remains as final fallback for self-hosters.

  // Detect whether Dodo / Razorpay are configured server-side and pick the
  // primary payment path.
  let DODO_ENABLED = false;
  let RZP_KEY_ID = null;
  let RZP_ENABLED = false;
  fetch('/api/version').then(r => r.ok ? r.json() : null).then(j => {
    if (!j) {
      const fb = $('#fallbackPay'); if (fb) fb.style.display = 'block';
      return;
    }
    DODO_ENABLED = !!j.dodo_enabled;
    RZP_ENABLED  = !!j.rzp_enabled;
    RZP_KEY_ID   = j.rzp_key_id || null;

    if (DODO_ENABLED) {
      // Dodo wins. Show Dodo button, hide Razorpay UI completely, hide
      // the manual fallback (Dodo handles UPI + card + netbanking).
      const dodoBtn = $('#dodoBtn'); if (dodoBtn) dodoBtn.style.display = 'block';
      const dodoNote = $('#dodoBtnNote'); if (dodoNote) dodoNote.style.display = 'block';
      const rzpBtn = $('#rzpBtn');           if (rzpBtn) rzpBtn.style.display = 'none';
      const rzpNote = $('#rzpBtnNote');      if (rzpNote) rzpNote.style.display = 'none';
      const sim  = $('#rzpSimulateBtn');     if (sim)  sim.style.display = 'none';
      const simNote = $('#rzpSimulateNote'); if (simNote) simNote.style.display = 'none';
      const hint = $('#testModeHint');       if (hint) hint.style.display = 'none';
      const fb = $('#fallbackPay');          if (fb) fb.style.display = 'none';
      return;
    }

    if (RZP_ENABLED && RZP_KEY_ID) {
      const fb = $('#fallbackPay'); if (fb) fb.style.display = 'none';
      // v1.4.4: in test mode, demote the (broken) Razorpay button and
      // promote the green simulate button to primary. Reveal the test hint.
      if (/^rzp_test_/.test(RZP_KEY_ID)) {
        const hint = $('#testModeHint');
        if (hint) hint.style.display = 'block';
        const sim  = $('#rzpSimulateBtn');
        const note = $('#rzpSimulateNote');
        const blue = $('#rzpBtn');
        const blueNote = $('#rzpBtnNote');
        if (sim)  sim.style.display  = 'block';
        if (note) note.style.display = 'block';
        if (blue) {
          blue.style.fontSize = '13.5px';
          blue.style.padding = '12px';
          blue.style.opacity = '0.55';
          blue.style.marginTop = '14px';
          blue.style.background = 'rgba(255,255,255,0.06)';
          blue.style.border = '1px solid rgba(255,255,255,0.12)';
          blue.textContent = '🔵 Try Razorpay sheet (may fail in test mode)';
        }
        if (blueNote) blueNote.style.display = 'none';
      }
    } else {
      const fb = $('#fallbackPay');
      if (fb) fb.style.display = 'block';
      const rzpBtn = $('#rzpBtn');
      if (rzpBtn) rzpBtn.style.display = 'none';
    }
  }).catch(() => {
    // If /api/version fails, fall back gracefully.
    const fb = $('#fallbackPay');
    if (fb) fb.style.display = 'block';
  });

  // v1.7.0 — Dodo button. Creates a hosted checkout session and redirects.
  $('#dodoBtn').addEventListener('click', async () => {
    if (!order) { alert('Lost order context. Please refresh.'); return; }
    const btn = $('#dodoBtn');
    btn.disabled = true;
    btn.textContent = 'Opening payment…';
    track('buy_dodo_clicked');
    try {
      const r = await fetch('/api/buy/dodo-checkout', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          order_id: order.order_id,
          contact: (order && order.contact) || '',
        }),
      });
      if (!r.ok) throw new Error(await r.text());
      const j = await r.json();
      if (j.already_paid) {
        startPolling();
        show('pending');
        return;
      }
      if (!j.checkout_url) throw new Error('dodo did not return a checkout url');
      // Persist the session id — it acts as the recovery code we'll
      // present to /api/buy/dodo-complete after webhook approval.
      if (j.session_id) {
        order.dodo_session_id = j.session_id;
        localStorage.setItem('rw_buy_order', JSON.stringify(order));
      }
      localStorage.setItem('rw_buy_pending', '1');
      // Flush analytics before navigating away.
      flushEvents(true);
      window.location.href = j.checkout_url;
    } catch (err) {
      alert('Could not start payment: ' + (err.message || err));
      btn.disabled = false;
      btn.textContent = '⚡ Pay ₹9 — instant unlock';
    }
  });

  $('#rzpBtn').addEventListener('click', async () => {
    if (!order) { alert('Lost order context. Please refresh.'); return; }
    if (!window.Razorpay) {
      alert('Razorpay checkout failed to load. Please retry, or use the manual UPI option below.');
      const fb = $('#fallbackPay');
      if (fb) fb.style.display = 'block';
      return;
    }
    $('#rzpBtn').disabled = true;
    $('#rzpBtn').textContent = 'Opening payment…';
    track('buy_rzp_clicked');
    try {
      const r = await fetch('/api/buy/rzp-order', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ order_id: order.order_id }),
      });
      if (!r.ok) {
        const txt = await r.text();
        throw new Error(txt || 'rzp-order failed');
      }
      const j = await r.json();
      if (j.already_paid) {
        // Re-poll status to retrieve token.
        startPolling();
        show('pending');
        return;
      }

      const opts = {
        key: j.key_id || RZP_KEY_ID,
        amount: j.amount,
        currency: j.currency || 'INR',
        order_id: j.rzp_order_id,
        name: 'Rewire',
        description: 'Premium · Lifetime',
        prefill: {
          email: (order && order.email) || '',
          contact: (order && order.contact) || '',
        },
        theme: { color: '#ff007a' },
        // v1.4.1: Drop the `method: { upi: true, card: false, ... }`
        // restriction. In Razorpay test mode (especially on desktop, where
        // UPI Intent doesn't apply directly), restricting to UPI-only can
        // leave the sheet empty if the merchant account doesn't have all
        // UPI flows enabled. Letting Razorpay show its default set (UPI +
        // Card + Netbanking + Wallet) is bulletproof: mobile users still
        // see UPI tiles prominently, desktop users get test-card / QR
        // fallbacks. UPI auto-shows first when the device supports it.
        handler: async (resp) => {
          // Success path. Verify on server, get token.
          track('buy_rzp_success', { extra: { rzp_payment_id: resp.razorpay_payment_id } });
          try {
            const vr = await fetch('/api/buy/rzp-verify', {
              method: 'POST', headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({
                order_id: order.order_id,
                rzp_order_id: resp.razorpay_order_id,
                rzp_payment_id: resp.razorpay_payment_id,
                rzp_signature: resp.razorpay_signature,
              }),
            });
            if (!vr.ok) throw new Error(await vr.text());
            const vj = await vr.json();
            if (vj.token) {
              localStorage.setItem('rw_premium_token', vj.token);
              $('#tokenBox').textContent = vj.token;
            }
            track('buy_token_unlocked');
            show('done');
          } catch (err) {
            alert('Payment succeeded but verification failed: ' + (err.message || err) +
                  '\n\nWe will reconcile and email your token to ' + ((order && order.email) || 'you') +
                  ' shortly. For urgent help: admin@gogillu.live');
            show('pending');
            startPolling();
          }
        },
        modal: {
          ondismiss: () => {
            track('buy_rzp_dismissed');
            $('#rzpBtn').disabled = false;
            $('#rzpBtn').textContent = '⚡ Pay ₹9 — instant unlock';
          },
        },
      };
      const rzp = new Razorpay(opts);
      rzp.on('payment.failed', (resp) => {
        track('buy_rzp_failed', { extra: { code: resp.error?.code, reason: resp.error?.reason } });
        alert('Payment failed: ' + (resp.error?.description || 'unknown') +
              '\n\nReason: ' + (resp.error?.reason || '') +
              '\n\nTry again, or use the manual UPI option below.');
        const fb = $('#fallbackPay');
        if (fb) fb.style.display = 'block';
        $('#rzpBtn').disabled = false;
        $('#rzpBtn').textContent = '⚡ Pay ₹9 — instant unlock';
      });
      rzp.open();
    } catch (err) {
      alert('Could not start payment: ' + (err.message || err));
      $('#rzpBtn').disabled = false;
      $('#rzpBtn').textContent = '⚡ Pay ₹9 — instant unlock';
      const fb = $('#fallbackPay');
      if (fb) fb.style.display = 'block';
    }
  });

  // v1.4.3: Test-mode simulate button. Synthesizes a valid HMAC server-side
  // and runs the full verify path. Mints a real lifetime token; refuses on
  // live keys server-side. Lets the user exercise end-to-end token issuance
  // without depending on Razorpay's UPI activation.
  $('#rzpSimulateBtn').addEventListener('click', async () => {
    if (!order) { alert('Lost order context. Please refresh.'); return; }
    const sim = $('#rzpSimulateBtn');
    sim.disabled = true;
    sim.textContent = 'Simulating…';
    track('buy_rzp_simulate_clicked');
    try {
      // First, ensure an rzp_order_id is bound to the local order.
      const ro = await fetch('/api/buy/rzp-order', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ order_id: order.order_id }),
      });
      if (!ro.ok) throw new Error('rzp-order: ' + (await ro.text()));
      // Now run the simulate.
      const r = await fetch('/api/buy/rzp-test-simulate', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ order_id: order.order_id }),
      });
      if (!r.ok) throw new Error('simulate: ' + (await r.text()));
      const j = await r.json();
      if (j.token) {
        localStorage.setItem('rw_premium_token', j.token);
        $('#tokenBox').textContent = j.token;
        track('buy_token_unlocked');
        show('done');
      } else {
        throw new Error('no token in response');
      }
    } catch (err) {
      alert('Simulation failed: ' + (err.message || err));
    } finally {
      sim.disabled = false;
      sim.textContent = '🧪 Simulate test payment (skip Razorpay sheet)';
    }
  });

  // Fallback chain (only visible if Razorpay not configured / failed).
  $('#upiBtn').addEventListener('click', () => track('buy_upi_app_clicked'));

  // v1.2: One-tap honor claim. Token is minted + emailed instantly; admin
  // reconciles offline.
  $('#claimBtn').addEventListener('click', async () => {
    if (!order) { alert('Lost order context. Please refresh.'); return; }
    $('#claimBtn').disabled = true;
    $('#claimBtn').textContent = 'Unlocking…';
    track('buy_claim_clicked');
    try {
      const r = await fetch('/api/buy/claim', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ order_id: order.order_id, utr: '' }),
      });
      if (r.status === 409) {
        const txt = await r.text();
        alert(txt || 'Already claimed.');
        return;
      }
      if (r.status === 429) {
        alert('Too many claims from this network. Please wait or contact admin@gogillu.live.');
        return;
      }
      if (!r.ok) throw new Error(await r.text());
      const j = await r.json();
      if (j.token) {
        localStorage.setItem('rw_premium_token', j.token);
        $('#tokenBox').textContent = j.token;
      } else {
        $('#tokenBox').textContent = '(token already retrieved — check your email)';
      }
      track('buy_token_unlocked');
      show('done');
    } catch (err) {
      alert('Unlock failed: ' + (err.message || err) + '\n\nIf you paid, write to admin@gogillu.live with order ' + (order && order.order_id) + '.');
    } finally {
      $('#claimBtn').disabled = false;
      $('#claimBtn').textContent = '✨ I\u0027ve paid — unlock now';
    }
  });

  // ---------- Step 4 — Pending → poll ----------
  $('#pollBtn').addEventListener('click', () => poll(true));
  // v1.7.0: two-tier polling. Right after returning from Dodo checkout we
  // poll every 2 s for the first 60 s (covers normal webhook latency),
  // then back off to 30 s for up to 10 min, then stop. Without this the
  // 30-s-only loop would leave the user staring at "pending" for half a
  // minute after a successful pay.
  let pollStartedAt = 0;
  function startPolling(fastMode) {
    if (pollTimer) return;
    pollStartedAt = Date.now();
    poll(false);
    schedulePoll(fastMode === true);
  }
  function schedulePoll(fast) {
    if (pollTimer) { clearTimeout(pollTimer); pollTimer = null; }
    const elapsed = Date.now() - pollStartedAt;
    if (elapsed > 10 * 60 * 1000) {
      // After 10 min, stop. User can press the manual button.
      return;
    }
    const interval = (fast && elapsed < 60 * 1000) ? 2000 : 30000;
    pollTimer = setTimeout(() => {
      poll(false).finally(() => schedulePoll(fast));
    }, interval);
  }
  async function poll(showAlert) {
    if (!order) return;
    try {
      const r = await fetch('/api/buy/status?order_id=' + encodeURIComponent(order.order_id));
      if (!r.ok) throw new Error(await r.text());
      const j = await r.json();
      track('buy_status_poll', { extra: { status: j.status } });
      if (j.status === 'approved') {
        if (pollTimer) { clearTimeout(pollTimer); clearInterval(pollTimer); pollTimer = null; }
        await completeAndUnlock();
      } else if (j.status === 'rejected') {
        if (pollTimer) { clearTimeout(pollTimer); clearInterval(pollTimer); pollTimer = null; }
        alert('Sadly your order was rejected. Please write to admin@gogillu.live with proof of payment.');
      } else if (showAlert) {
        alert('Still pending — check back soon. We notify by email too.');
      }
    } catch (err) {
      if (showAlert) alert('Status check failed: ' + (err.message || err));
    }
  }
  async function completeAndUnlock() {
    // v1.7.0: choose completion endpoint based on which provider issued
    // the order. Dodo orders carry a dodo_session_id (set when /buy/dodo-checkout
    // returned). Razorpay & manual orders use the legacy UTR-based path.
    const dodoSession = (order && order.dodo_session_id) || '';
    if (dodoSession) {
      try {
        const r = await fetch('/api/buy/dodo-complete', {
          method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ order_id: order.order_id, session_id: dodoSession }),
        });
        if (!r.ok) throw new Error(await r.text());
        const j = await r.json();
        if (j.token) {
          localStorage.setItem('rw_premium_token', j.token);
          $('#tokenBox').textContent = j.token;
          track('buy_token_unlocked');
        } else {
          $('#tokenBox').textContent = '(token already retrieved — check your email)';
        }
        localStorage.removeItem('rw_buy_pending');
        show('done');
      } catch (err) {
        alert('Approved! But token fetch failed. Check your email — the token has been mailed.');
        show('done');
      }
      return;
    }
    const utr = localStorage.getItem('rw_buy_utr') || '';
    try {
      const r = await fetch('/api/buy/complete', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ order_id: order.order_id, utr }),
      });
      const j = await r.json();
      if (j.token) {
        localStorage.setItem('rw_premium_token', j.token);
        $('#tokenBox').textContent = j.token;
        track('buy_token_unlocked');
      } else {
        $('#tokenBox').textContent = '(token already retrieved — check your email)';
      }
      show('done');
    } catch (err) {
      alert('Approved! But token fetch failed. Check your email — the token has been mailed.');
      show('done');
    }
  }
  $('#goPremium').addEventListener('click', () => {
    track('buy_open_premium');
    window.location.href = '/premium';
  });

  // ---------- Resume on reload ----------
  // v1.2: with the one-tap claim flow, resume just drops the user back on
  // the pay screen (with the order context restored). If they already
  // unlocked and have a token, /premium handles that and we don't need
  // to do anything special here.
  // v1.7.0: also handles return-from-Dodo. Dodo's hosted checkout
  // redirects to /buy?dodo_order=<localid> on completion. We trust the
  // server-side webhook to flip the status (no signed client payload);
  // the frontend just polls /api/buy/status until approved.
  (function resume() {
    let returnFromDodo = false;
    try {
      const params = new URLSearchParams(window.location.search);
      const dodoOrderInUrl = params.get('dodo_order');
      if (dodoOrderInUrl) {
        returnFromDodo = true;
        // Strip the query string from the address bar so a reload
        // doesn't re-trigger this branch.
        const cleanUrl = window.location.pathname + window.location.hash;
        try { window.history.replaceState({}, '', cleanUrl); } catch {}
      }
    } catch {}

    const saved = localStorage.getItem('rw_buy_order');
    if (!saved) return;
    try {
      const o = JSON.parse(saved);
      if (!o.expires_at || Date.now() > o.expires_at) {
        localStorage.removeItem('rw_buy_order');
        localStorage.removeItem('rw_buy_utr');
        localStorage.removeItem('rw_buy_pending');
        return;
      }
      order = o;
      $('#orderRef').textContent = o.order_id;
      $('#upiBtn').href = o.deep_link;
      $('#qrImg').src = '/api/buy/qr?order_id=' + encodeURIComponent(o.order_id);

      // If we're returning from Dodo, jump straight to the pending screen
      // and start fast-polling — the webhook may already have approved the
      // order by the time we land here, or it'll arrive within seconds.
      if (returnFromDodo) {
        $('#pendingOrder').textContent = o.order_id;
        show('pending');
        startPolling(true);
        track('buy_dodo_return');
        return;
      }

      // Check status — if already approved, skip directly to done.
      fetch('/api/buy/status?order_id=' + encodeURIComponent(o.order_id))
        .then(r => r.ok ? r.json() : null)
        .then(j => {
          if (j && (j.status === 'approved' || j.status === 'pending_verify') &&
              localStorage.getItem('rw_premium_token')) {
            $('#tokenBox').textContent = localStorage.getItem('rw_premium_token');
            show('done');
            return;
          }
          // If status is already approved but we don't have the token,
          // try to fetch it. (Edge case: user closed the tab right after
          // Dodo webhook approval.)
          if (j && j.status === 'approved') {
            completeAndUnlock();
            return;
          }
          // Pending: if we have a hint we're mid-flow from Dodo, poll.
          if (localStorage.getItem('rw_buy_pending') === '1') {
            $('#pendingOrder').textContent = o.order_id;
            show('pending');
            // Was mid-checkout — assume Dodo and poll fast.
            startPolling(true);
            return;
          }
          show('pay');
        })
        .catch(() => show('pay'));
    } catch {}
  })();
})();
