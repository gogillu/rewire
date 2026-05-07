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
  $('#goPay').addEventListener('click', async () => {
    const e1 = $('#email').value.trim().toLowerCase();
    const e2 = $('#email2').value.trim().toLowerCase();
    if (!/^[^@\s]+@[^@\s]+\.[^@\s]+$/.test(e1)) { alert('Please enter a valid email.'); return; }
    if (e1 !== e2) { alert("Emails don't match — please retype."); return; }
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
      localStorage.setItem('rw_buy_order', JSON.stringify(order));
      $('#orderRef').textContent = order.order_id;
      $('#vpa').textContent = order.vpa;
      $('#upiBtn').href = order.deep_link;
      track('buy_order_created', { extra: { order_id: order.order_id } });
      show('pay');
    } catch (err) {
      alert('Could not start order: ' + (err.message || err));
    } finally {
      $('#goPay').disabled = false;
      $('#goPay').textContent = 'Continue to UPI payment →';
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
  $('#copyVpa').addEventListener('click', async () => {
    try {
      await navigator.clipboard.writeText($('#vpa').textContent);
      $('#copyVpa').textContent = 'Copied!';
      setTimeout(() => { $('#copyVpa').textContent = 'Copy'; }, 1500);
    } catch {}
  });
  $('#upiBtn').addEventListener('click', () => track('buy_upi_app_clicked'));

  // v1.2: One-tap honor claim. Token is minted + emailed instantly; admin
  // reconciles offline. Optional UTR (collapsed in <details>) helps verification.
  $('#claimBtn').addEventListener('click', async () => {
    if (!order) { alert('Lost order context. Please refresh.'); return; }
    let utr = ($('#utr')?.value || '').trim().toUpperCase().replace(/[^A-Z0-9]/g, '');
    if (utr && (utr.length < 10 || utr.length > 30)) {
      alert('UTR must be 10–30 letters/digits, or leave it blank.');
      return;
    }
    $('#claimBtn').disabled = true;
    $('#claimBtn').textContent = 'Unlocking…';
    track('buy_claim_clicked', { extra: { has_utr: utr ? 1 : 0 } });
    try {
      const r = await fetch('/api/buy/claim', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ order_id: order.order_id, utr }),
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
      if (utr) localStorage.setItem('rw_buy_utr', utr);
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
  function startPolling() {
    if (pollTimer) return;
    poll(false);
    pollTimer = setInterval(() => poll(false), 30000);
  }
  async function poll(showAlert) {
    if (!order) return;
    try {
      const r = await fetch('/api/buy/status?order_id=' + encodeURIComponent(order.order_id));
      if (!r.ok) throw new Error(await r.text());
      const j = await r.json();
      track('buy_status_poll', { extra: { status: j.status } });
      if (j.status === 'approved') {
        if (pollTimer) { clearInterval(pollTimer); pollTimer = null; }
        await completeAndUnlock();
      } else if (j.status === 'rejected') {
        if (pollTimer) { clearInterval(pollTimer); pollTimer = null; }
        alert('Sadly your order was rejected. Please write to admin@gogillu.live with proof of payment.');
      } else if (showAlert) {
        alert('Still pending — check back soon. We notify by email too.');
      }
    } catch (err) {
      if (showAlert) alert('Status check failed: ' + (err.message || err));
    }
  }
  async function completeAndUnlock() {
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
  (function resume() {
    const saved = localStorage.getItem('rw_buy_order');
    if (!saved) return;
    try {
      const o = JSON.parse(saved);
      if (!o.expires_at || Date.now() > o.expires_at) {
        localStorage.removeItem('rw_buy_order');
        localStorage.removeItem('rw_buy_utr');
        return;
      }
      order = o;
      $('#orderRef').textContent = o.order_id;
      $('#vpa').textContent = o.vpa;
      $('#upiBtn').href = o.deep_link;
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
          show('pay');
        })
        .catch(() => show('pay'));
    } catch {}
  })();
})();
