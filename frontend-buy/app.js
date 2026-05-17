// Rewire /buy — v1.8.2 production payment flow.
//
// Two entry paths:
//
//   (a) Fresh visit (?dodo_order=… absent in URL):
//       Show payment selection card. User enters email → click "Pay ₹49".
//       Frontend: POST /api/buy/init { email, anon_id, session_id }
//         → { order_id }
//       Frontend: POST /api/buy/dodo-checkout { order_id }
//         → { checkout_url, session_id }
//       Persist {order_id, session_id, email} in localStorage so the
//       polling phase can recover them even if Dodo strips query params.
//       Redirect: window.location = checkout_url.
//
//   (b) Return from Dodo (?dodo_order=<id> in URL):
//       Show "Confirming payment…" spinner. Start polling
//       /api/buy/status?order_id=<id> every 2s for the first 30s,
//       then every 5s up to 5 minutes.
//         status='approved'  → POST /api/buy/dodo-complete { order_id, session_id }
//                              → { token }
//                              → save to localStorage (rw_premium_token)
//                              → window.location.replace('/premium?t=' + token)
//         status='rejected'  → show failure card.
//         polling timeout    → show failure card with admin contact info.
//
// No user button mints a token. No raw VPA touches the DOM. No client-state
// is trusted. The backend's Dodo webhook is the sole approver.

(function () {
  'use strict';

  var REWIRE_VERSION = '1.8.2';

  // ------- DOM helpers -------
  var $ = function (id) { return document.getElementById(id); };
  function show(stage) {
    ['stage-select', 'stage-verify', 'stage-success', 'stage-fail']
      .forEach(function (s) { $(s).classList.add('hidden'); });
    $(stage).classList.remove('hidden');
  }
  function showErr(elId, msg) {
    var el = $(elId);
    el.textContent = msg;
    el.classList.remove('hidden');
  }
  function hideErr(elId) {
    var el = $(elId);
    el.textContent = '';
    el.classList.add('hidden');
  }

  // ------- session ids -------
  // anon_id is sticky per browser; session_id rotates per buy-flow attempt.
  function getOrMakeAnonId() {
    try {
      var k = 'rw_anon_id';
      var v = localStorage.getItem(k);
      if (!v) {
        v = 'a-' + Math.random().toString(36).slice(2, 10) + Date.now().toString(36);
        localStorage.setItem(k, v);
      }
      return v;
    } catch (_) { return 'a-x'; }
  }
  function newSessionId() {
    return 's-' + Math.random().toString(36).slice(2, 10) + Date.now().toString(36);
  }

  // ------- pending order persistence -------
  // We keep the current attempt's {order_id, session_id, email} in
  // localStorage under rw_pending_buy. Dodo *should* echo the order id
  // back via ?dodo_order, but localStorage is the source of truth and
  // also carries the session_id (which Dodo never sees), needed by
  // /api/buy/dodo-complete for one-time-pickup security.
  var PENDING_KEY = 'rw_pending_buy';
  function setPending(p) {
    try { localStorage.setItem(PENDING_KEY, JSON.stringify(p)); } catch (_) {}
  }
  function getPending() {
    try { return JSON.parse(localStorage.getItem(PENDING_KEY) || '{}'); }
    catch (_) { return {}; }
  }
  function clearPending() {
    try { localStorage.removeItem(PENDING_KEY); } catch (_) {}
  }

  // ------- URL params -------
  function getParam(name) {
    var p = new URLSearchParams(window.location.search);
    return (p.get(name) || '').trim();
  }

  // ------- HTTP helper -------
  function postJSON(url, body) {
    return fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body || {}),
      credentials: 'same-origin',
    }).then(function (r) {
      return r.text().then(function (t) {
        var j = null;
        try { j = t ? JSON.parse(t) : null; } catch (_) { /* not json */ }
        return { ok: r.ok, status: r.status, body: j, text: t };
      });
    });
  }
  function getJSON(url) {
    return fetch(url, { credentials: 'same-origin', cache: 'no-store' })
      .then(function (r) {
        return r.text().then(function (t) {
          var j = null;
          try { j = t ? JSON.parse(t) : null; } catch (_) { }
          return { ok: r.ok, status: r.status, body: j, text: t };
        });
      });
  }

  // ------- already-premium short-circuit -------
  function existingTokenOrNull() {
    try {
      var t = (localStorage.getItem('rw_premium_token') || '').trim();
      return t || null;
    } catch (_) { return null; }
  }

  // ============================================================
  // STAGE A — Pay button flow
  // ============================================================

  function wireSelectStage() {
    var btn = $('pay-btn');
    var input = $('email-input');

    // Prefill email if we have one from a prior attempt.
    var pending = getPending();
    if (pending && pending.email) input.value = pending.email;

    function submit() {
      hideErr('select-err');
      var email = (input.value || '').trim();
      if (!email || !/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email)) {
        showErr('select-err', 'Please enter a valid email address.');
        input.focus();
        return;
      }
      btn.disabled = true;
      btn.textContent = 'Creating order…';

      var anonId = getOrMakeAnonId();
      var sessionId = newSessionId();

      postJSON('/api/buy/init', {
        email: email,
        anon_id: anonId,
        session_id: sessionId,
      }).then(function (r) {
        if (!r.ok || !r.body || !r.body.order_id) {
          throw new Error(r.text || ('init failed: HTTP ' + r.status));
        }
        var orderId = r.body.order_id;
        var expiresAt = r.body.expires_at || (Date.now() + 24 * 60 * 60 * 1000);
        // Persist BEFORE going to Dodo so the return-from-Dodo flow can
        // recover even if the user closes/reopens the tab.
        setPending({
          order_id: orderId,
          session_id: sessionId,
          email: email,
          expires_at: expiresAt,
          ts: Date.now(),
        });
        // Wire the home-page abandonment nudge (see frontend/app.js).
        // It looks for rw_buy_pending_nudge { order_id, expires_at } and
        // pops a sticky banner on / if the user never finishes payment.
        try {
          localStorage.setItem('rw_buy_pending_nudge', JSON.stringify({
            order_id: orderId,
            expires_at: expiresAt,
            email: email,
          }));
          localStorage.removeItem('rw_buy_nudge_off');
        } catch (_) { }

        btn.textContent = 'Opening secure checkout…';
        return postJSON('/api/buy/dodo-checkout', {
          order_id: orderId,
          contact: email,
        }).then(function (r2) {
          if (!r2.ok || !r2.body) {
            throw new Error(r2.text || ('checkout failed: HTTP ' + r2.status));
          }
          if (r2.body.already_paid) {
            // Defensive — the same email/order already approved.
            // Drive through the verify stage which will fetch the token.
            window.location.replace('/buy?dodo_order=' + encodeURIComponent(orderId));
            return;
          }
          if (!r2.body.checkout_url) {
            throw new Error('Checkout URL missing from server response.');
          }
          // Pin the Dodo session id (different from our local session id)
          // alongside the local session id we use for /dodo-complete.
          var p = getPending();
          p.dodo_session_id = r2.body.session_id || '';
          setPending(p);
          // Go to Dodo's hosted page.
          window.location.assign(r2.body.checkout_url);
        });
      }).catch(function (e) {
        btn.disabled = false;
        btn.textContent = 'Pay ₹49 →';
        var msg = (e && e.message ? e.message : 'Could not start checkout.').slice(0, 280);
        showErr('select-err', msg);
      });
    }

    btn.addEventListener('click', submit);
    input.addEventListener('keydown', function (e) {
      if (e.key === 'Enter') submit();
    });
  }

  // ============================================================
  // STAGE B — Return-from-Dodo verification
  // ============================================================

  function pollStatus(orderId, sessionId, email) {
    show('stage-verify');
    var verifyMsg = $('verify-msg');

    // Polling schedule: fast (every 2s) for the first 30s, then slower
    // (every 5s) up to 5 minutes total. Web-hooks deliver in 1-5s
    // typically; the long tail covers UPI bank settlement delay.
    var fastUntil = Date.now() + 30 * 1000;
    var hardStop = Date.now() + 5 * 60 * 1000;
    var attempts = 0;
    var stopped = false;

    function stop() { stopped = true; }

    function tick() {
      if (stopped) return;
      attempts++;
      if (attempts === 5) {
        verifyMsg.textContent =
          'Still waiting for confirmation from your bank — this is normal for UPI, ' +
          'usually under a minute.';
      }
      if (attempts === 30) {
        verifyMsg.textContent =
          'Taking longer than usual. Hang tight — we\u2019ll redirect you the ' +
          'instant the payment confirms.';
      }

      getJSON('/api/buy/status?order_id=' + encodeURIComponent(orderId))
        .then(function (r) {
          if (stopped) return;
          if (!r.ok || !r.body) {
            // Network blip — keep polling silently.
            return scheduleNext();
          }
          var st = (r.body.status || '').toLowerCase();
          if (st === 'approved') {
            stop();
            return fetchTokenAndRedirect(orderId, sessionId);
          }
          if (st === 'rejected' || st === 'expired') {
            stop();
            return showFailure(orderId, email,
              'Your payment was ' + st + '. If you were charged, ' +
              'please email admin@gogillu.live.');
          }
          // 'initiated' / 'pending_verify' / 'utr_submitted' → keep polling.
          scheduleNext();
        })
        .catch(function () {
          if (!stopped) scheduleNext();
        });
    }

    function scheduleNext() {
      if (stopped) return;
      if (Date.now() > hardStop) {
        stop();
        return showFailure(orderId, email,
          'We didn\u2019t hear back from Dodo within 5 minutes. If you ' +
          'completed the payment, your token will arrive by email — ' +
          'no further action needed.');
      }
      var interval = Date.now() < fastUntil ? 2000 : 5000;
      setTimeout(tick, interval);
    }

    // Kick off immediately (don't wait 2s for first poll — webhook may
    // already have landed before the redirect even returned).
    tick();
  }

  function fetchTokenAndRedirect(orderId, sessionId) {
    postJSON('/api/buy/dodo-complete', {
      order_id: orderId,
      session_id: sessionId,
    }).then(function (r) {
      if (!r.ok || !r.body || !r.body.token) {
        // Token already picked up (one-time delivery) OR status drift.
        // Either way the order is approved — direct the user to /premium
        // and let the recovery flow there handle it.
        show('stage-success');
        clearPending();
        try { localStorage.removeItem('rw_buy_pending_nudge'); } catch (_) { }
        return setTimeout(function () {
          window.location.replace('/premium');
        }, 600);
      }
      try {
        localStorage.setItem('rw_premium_token', r.body.token);
        localStorage.removeItem(PENDING_KEY);
        localStorage.removeItem('rw_buy_pending_nudge');
      } catch (_) {}
      show('stage-success');
      setTimeout(function () {
        window.location.replace('/premium?t=' + encodeURIComponent(r.body.token));
      }, 700);
    }).catch(function (e) {
      // Approved server-side but we couldn't pull the token client-side
      // — degrade gracefully to /premium where the recovery flow runs.
      show('stage-success');
      setTimeout(function () { window.location.replace('/premium'); }, 800);
    });
  }

  function showFailure(orderId, email, msg) {
    show('stage-fail');
    $('fail-order').textContent = orderId || '—';
    if (email) $('fail-email').textContent = email;
    if (msg) {
      var verr = $('verify-err');
      verr.textContent = msg;
      verr.classList.remove('hidden');
    }
    $('fail-retry').addEventListener('click', function () {
      clearPending();
      // Clean URL of dodo_order so a fresh attempt starts in stage-select.
      var u = new URL(window.location.href);
      u.searchParams.delete('dodo_order');
      window.location.replace(u.toString());
    }, { once: true });
  }

  // ============================================================
  // entry
  // ============================================================

  function init() {
    window.REWIRE_VERSION = REWIRE_VERSION;

    // If user already has a premium token, bypass the buy flow entirely.
    var existing = existingTokenOrNull();
    if (existing) {
      window.location.replace('/premium?t=' + encodeURIComponent(existing));
      return;
    }

    var dodoOrder = getParam('dodo_order');
    if (dodoOrder) {
      // Returning from Dodo. Reconcile with localStorage pending state.
      var pending = getPending();
      var orderId = dodoOrder;
      var sessionId = (pending && pending.order_id === orderId && pending.session_id) || '';
      var email = (pending && pending.order_id === orderId && pending.email) || '';
      if (!sessionId) {
        // Tab was closed between checkout creation and return — we lost
        // the session_id needed by /dodo-complete. Polling /status still
        // gives us status='approved' and the webhook still emails the
        // token; we just can't auto-pick-up. Show success → /premium
        // and let server-side / email handle it.
        pollStatusNoSession(orderId, email);
        return;
      }
      pollStatus(orderId, sessionId, email);
      return;
    }

    // Default: payment selection.
    show('stage-select');
    wireSelectStage();
  }

  // pollStatusNoSession is the degraded path when we lack the local
  // session_id (e.g. user cleared storage). Same polling loop but on
  // approval we just go to /premium and surface email-delivery messaging.
  function pollStatusNoSession(orderId, email) {
    show('stage-verify');
    $('verify-msg').textContent =
      'Your browser doesn\u2019t have the session token for this order — ' +
      'we\u2019ll still confirm your payment server-side and email you the ' +
      'access link.';
    var hardStop = Date.now() + 5 * 60 * 1000;
    (function tick() {
      if (Date.now() > hardStop) {
        return showFailure(orderId, email,
          'We didn\u2019t hear back from Dodo within 5 minutes. If you ' +
          'completed the payment, the lifetime token will be emailed to you.');
      }
      getJSON('/api/buy/status?order_id=' + encodeURIComponent(orderId))
        .then(function (r) {
          var st = r && r.body && (r.body.status || '').toLowerCase();
          if (st === 'approved') {
            show('stage-success');
            return setTimeout(function () {
              window.location.replace('/premium');
            }, 800);
          }
          if (st === 'rejected' || st === 'expired') {
            return showFailure(orderId, email,
              'Your payment was ' + st + '. If you were charged, ' +
              'please email admin@gogillu.live.');
          }
          setTimeout(tick, 3000);
        })
        .catch(function () { setTimeout(tick, 3000); });
    })();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
