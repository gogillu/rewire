// Rewire frontend — vanilla JS. Random shuffle on every page load,
// local /audio/<id>.mp3 playback (loop), persistent like history.
//
// v1.3.0: bigger Instagram-style stats badge, "+" FAB community entry,
// full-modal premium sales-pitch (with bullet benefits), prominent UPI
// deeplink CTA on /buy (avoids PhonePe gallery-QR security limits).
window.REWIRE_VERSION = '1.3.1';
console.log('[Rewire] frontend v' + window.REWIRE_VERSION);
(function () {
  'use strict';

  const $  = (sel, root = document) => root.querySelector(sel);
  const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

  // ---------- IndexedDB ----------
  const DB_NAME = 'rewire';
  const DB_VER  = 2;
  function openDB() {
    return new Promise((resolve, reject) => {
      const req = indexedDB.open(DB_NAME, 2);
      req.onupgradeneeded = (ev) => {
        const db = req.result;
        if (!db.objectStoreNames.contains('likes')) {
          db.createObjectStore('likes', { keyPath: 'movieId' });
        }
        if (!db.objectStoreNames.contains('cache')) {
          db.createObjectStore('cache', { keyPath: 'k' });
        }
        if (!db.objectStoreNames.contains('flags')) {
          // Key = movieId — one (latest) flag per movie is enough for the
          // "concerns already shared" toast on re-tap.
          db.createObjectStore('flags', { keyPath: 'movieId' });
        }
      };
      req.onerror = () => reject(req.error);
      req.onsuccess = () => resolve(req.result);
    });
  }
  async function idbGet(store, key) {
    const db = await openDB();
    return new Promise((res, rej) => {
      const tx = db.transaction(store, 'readonly');
      const r = tx.objectStore(store).get(key);
      r.onsuccess = () => res(r.result);
      r.onerror = () => rej(r.error);
    });
  }
  async function idbPut(store, val) {
    const db = await openDB();
    return new Promise((res, rej) => {
      const tx = db.transaction(store, 'readwrite');
      tx.objectStore(store).put(val);
      tx.oncomplete = () => res();
      tx.onerror = () => rej(tx.error);
    });
  }
  async function idbAll(store) {
    const db = await openDB();
    return new Promise((res, rej) => {
      const tx = db.transaction(store, 'readonly');
      const r = tx.objectStore(store).getAll();
      r.onsuccess = () => res(r.result || []);
      r.onerror = () => rej(r.error);
    });
  }

  // ---------- State ----------
  const state = {
    movies: [],            // shuffled order
    cardEls: [],
    likeMap: new Map(),    // movieId -> endingId
    flagMap: new Map(),    // movieId -> { reason, customText, ts }
    audioOn: false,
    audio: null,           // single HTMLAudio element shared across cards
    activeIdx: -1,
    currentSrc: null,
    activeStartedAt: 0,    // ms timestamp current card became active
    cardsViewed: 0,        // monotonic count, used to gate feedback widget
    pendingFlagMovie: null,
    nudgeDismissedAt: 0,
    // v1.2 community
    communityMovie: null,                  // currently-open movie in sheet
    communityLikes: new Set(),             // ending_ids the user has liked
    communityRatings: new Map(),           // ending_id -> 1..5
  };

  // ---------- Telemetry ----------
  // Per-tab uuid (sessionStorage) + per-device uuid (localStorage). The
  // device uuid lets us count distinct visitors without any login.
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
  const ENV = detectClient();
  function detectClient() {
    const ua = navigator.userAgent || '';
    let os='unknown', browser='unknown', device='desktop';
    if (/Android/i.test(ua)) { os='Android'; device='mobile'; }
    else if (/iPad/i.test(ua) || (/Macintosh/i.test(ua) && navigator.maxTouchPoints>1)) { os='iPadOS'; device='tablet'; }
    else if (/iPhone|iPod/i.test(ua)) { os='iOS'; device='mobile'; }
    else if (/Windows/i.test(ua)) os='Windows';
    else if (/Mac OS X/i.test(ua)) os='macOS';
    else if (/Linux/i.test(ua)) os='Linux';
    if (/Edg\//.test(ua)) browser='Edge';
    else if (/SamsungBrowser/.test(ua)) browser='Samsung';
    else if (/OPR\//.test(ua)) browser='Opera';
    else if (/Chrome\//.test(ua) && !/Chromium/.test(ua)) browser='Chrome';
    else if (/Firefox\//.test(ua)) browser='Firefox';
    else if (/Safari\//.test(ua) && !/Chrome|Chromium|Edg|OPR/.test(ua)) browser='Safari';
    if (device==='desktop' && /Mobi|Android/i.test(ua)) device='mobile';
    return { os, browser, device };
  }
  const telemetryQueue = [];
  function track(type, fields = {}) {
    telemetryQueue.push({
      ts: Date.now(),
      type,
      movie_id: fields.movie_id,
      ending_id: fields.ending_id,
      duration_ms: fields.duration_ms,
      audio_playing: state.audioOn ? 1 : 0,
      extra: fields.extra,
    });
    if (telemetryQueue.length >= 20) flushTelemetry();
  }
  let flushing = false;
  async function flushTelemetry(useBeacon = false) {
    if (flushing || telemetryQueue.length === 0) return;
    flushing = true;
    const batch = telemetryQueue.splice(0, telemetryQueue.length);
    const body = JSON.stringify({
      session_id: SESSION_ID, anon_id: ANON_ID,
      os: ENV.os, browser: ENV.browser, device: ENV.device,
      screen_w: window.innerWidth, screen_h: window.innerHeight,
      events: batch,
      mode: 'direct',
    });
    try {
      if (useBeacon && navigator.sendBeacon) {
        navigator.sendBeacon('/api/events', new Blob([body], { type: 'application/json' }));
      } else {
        await fetch('/api/events', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body, keepalive: true,
        });
      }
    } catch {
      // Drop on transient error; we already removed them from the queue
      // since the next flush should pick up live state anyway.
    } finally {
      flushing = false;
    }
  }
  setInterval(() => flushTelemetry(false), 5000);
  window.addEventListener('pagehide', () => {
    // Final dwell-time for whatever card is active.
    if (state.activeIdx >= 0 && state.activeStartedAt) {
      const m = state.movies[state.activeIdx];
      if (m) {
        telemetryQueue.push({
          ts: Date.now(), type: 'impression',
          movie_id: m.id,
          duration_ms: Date.now() - state.activeStartedAt,
          audio_playing: state.audioOn ? 1 : 0,
        });
      }
    }
    track('session_end');
    flushTelemetry(true);
  });
  window.addEventListener('visibilitychange', () => {
    track(document.hidden ? 'tab_hidden' : 'tab_visible');
    if (document.hidden) flushTelemetry(false);
  });
  // Initial session_start.
  track('session_start', { extra: { ref: document.referrer || '', tz: Intl.DateTimeFormat().resolvedOptions().timeZone || '' } });

  // ---------- API ----------
  async function fetchMovies() {
    // Try cached payload first for instant first paint, then refresh.
    const cached = await idbGet('cache', 'movies');
    if (cached && Date.now() - cached.t < 1000 * 60 * 30) {
      applyMoviePayload(cached.v);
      refresh().catch(() => {});
      return;
    }
    await refresh();
  }
  async function refresh() {
    const r = await fetch('/api/movies', { cache: 'no-store' });
    if (!r.ok) throw new Error('fetch movies failed');
    const data = await r.json();
    await idbPut('cache', { k: 'movies', v: data, t: Date.now() });
    applyMoviePayload(data);
  }
  function applyMoviePayload(payload) {
    const arr = (payload.movies || []).slice();
    // Random shuffle on every page-load — server returns deterministic
    // sort_order, we randomize per-tab.
    shuffleInPlace(arr);
    state.poolSrc = (payload.movies || []).slice();
    state.movies = arr;
    render();
    // Pre-warm the next 5 audio clips (and posters) so scrolling is smooth
    for (let i = 0; i < 5; i++) preload(arr[i]);
  }

  function appendShuffledBatch() {
    if (!state.poolSrc || !state.poolSrc.length) return;
    const batch = state.poolSrc.slice();
    shuffleInPlace(batch);
    const offset = state.movies.length;
    state.movies.push(...batch);
    const deck = $('#deck');
    const frag = document.createDocumentFragment();
    batch.forEach((m, i) => {
      const card = buildCard(m, offset + i);
      frag.appendChild(card);
      state.cardEls.push(card);
    });
    deck.appendChild(frag);
    // Observe the new cards.
    if (state._io) state.cardEls.slice(offset).forEach(el => state._io.observe(el));
  }
  async function postLike(endingId, delta) {
    try {
      const r = await fetch('/api/like', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ ending_id: endingId, delta }),
      });
      if (!r.ok) return null;
      return await r.json();
    } catch { return null; }
  }

  // ---------- Likes ----------
  async function loadLikes() {
    const all = await idbAll('likes');
    for (const row of all) state.likeMap.set(row.movieId, row.endingId);
  }

  // ---------- Render ----------
  function render() {
    const deck = $('#deck');
    deck.innerHTML = '';
    state.cardEls = [];
    if (!state.movies.length) {
      $('#empty').classList.add('show');
      return;
    }
    $('#empty').classList.remove('show');
    const frag = document.createDocumentFragment();
    state.movies.forEach((m, i) => {
      const card = buildCard(m, i);
      frag.appendChild(card);
      state.cardEls.push(card);
    });
    deck.appendChild(frag);
    setupObserver();
    requestAnimationFrame(() => state.cardEls[0]?.classList.add('ready'));
  }

  function buildCard(m, idx) {
    const card = document.createElement('section');
    card.className = 'card';
    card.dataset.idx = idx;
    card.dataset.movie = m.id;

    const poster = document.createElement('div');
    poster.className = 'poster';
    if (m.poster_url) poster.style.backgroundImage = `url("${m.poster_url}")`;
    else poster.style.background = generateGradient(m.id);
    card.appendChild(poster);

    const vig = document.createElement('div');
    vig.className = 'vignette';
    card.appendChild(vig);

    // TOP — movie title + year + IMDb rating + v1.3 stats badge (Insta-bigger)
    const top = document.createElement('div');
    top.className = 'top';
    const views = m.views || 0;
    const likesTotal = m.likes_total || 0;
    const conv = m.conversion_pct || 0;
    const showStats = views > 0 || likesTotal > 0;
    top.innerHTML = `
      <div class="title">${escapeHTML(m.title)}</div>
      <div class="sub">${m.year || ''} · ★ ${(m.imdb_rating || 0).toFixed(1)}</div>
      ${showStats ? `<div class="stats">
        <span><em>👁</em><b>${formatLikes(views)}</b></span>
        <span><em>❤</em><b>${formatLikes(likesTotal)}</b></span>
        ${conv > 0 ? `<span><em>⚡</em><b>${conv}%</b></span>` : ''}
      </div>` : ''}
    `;
    card.appendChild(top);

    // BOTTOM — three endings stacked just above the brand chip.
    const wrap = document.createElement('div');
    wrap.className = 'endings';
    const liked = state.likeMap.get(m.id);
    const slots = [0, 1, 2].map(i => m.endings && m.endings[i]);
    slots.forEach((e, i) => {
      const el = document.createElement('div');
      if (!e) {
        el.className = 'ending placeholder';
        el.innerHTML = `<div class="text">Rewriting reality…</div>`;
      } else {
        el.className = 'ending';
        if (liked === e.id) el.classList.add('liked');
        el.dataset.endingId = e.id;
        el.innerHTML = `
          <div class="text">${escapeHTML(e.text)}</div>
          <div class="row">
            <i class="heart"></i><span class="likes">${formatLikes(e.likes || 0)}</span>
          </div>
        `;
        el.addEventListener('click', (ev) => {
          ev.preventDefault();
          onLike(m.id, e.id, el, card, idx);
        });
      }
      wrap.appendChild(el);
    });
    card.appendChild(wrap);

    // VERY BOTTOM — Rewire brand chip
    const brand = document.createElement('div');
    brand.className = 'brand';
    brand.innerHTML = `<div class="logo">Rewire</div>`;
    card.appendChild(brand);

    // Flag button (top-right of card, just left of audio toggle).
    const flag = document.createElement('button');
    flag.className = 'flag-btn';
    if (state.flagMap.has(m.id)) flag.classList.add('flagged');
    flag.setAttribute('aria-label', 'Report a problem');
    flag.innerHTML = `<svg viewBox="0 0 24 24"><path d="M14.4 6L14 4H5v17h2v-7h5.6l.4 2h7V6z"/></svg>`;
    flag.addEventListener('click', (ev) => {
      ev.stopPropagation();
      onFlagClick(m.id);
    });
    card.appendChild(flag);

    // v1.3 community FAB — small "+" circular button, bottom-right corner.
    // Doesn't overlap the endings stack (the v1.2 wide pill did).
    const ccount = (m.community && m.community.length) || 0;
    const fab = document.createElement('button');
    fab.className = 'community-fab';
    fab.setAttribute('aria-label', ccount + ' community endings — add yours');
    fab.title = 'Community endings · add yours';
    fab.innerHTML = `<span class="plus">+</span>` +
      (ccount > 0 ? `<span class="badge">${ccount > 99 ? '99+' : ccount}</span>` : '');
    fab.addEventListener('click', (ev) => {
      ev.stopPropagation();
      openCommunitySheet(m);
    });
    card.appendChild(fab);

    return card;
  }

  function formatLikes(n) {
    if (n < 1000) return String(n);
    if (n < 1_000_000) return (n / 1000).toFixed(n < 10_000 ? 1 : 0) + 'k';
    return (n / 1_000_000).toFixed(1) + 'M';
  }

  // ---------- Like ----------
  async function onLike(movieId, endingId, el, card, idx) {
    const prev = state.likeMap.get(movieId);
    if (prev === endingId) {
      // Tap on already-liked → advance to next without changing the like.
      advanceTo(idx + 1);
      return;
    }
    state.likeMap.set(movieId, endingId);
    $$('.ending', card).forEach(x => x.classList.remove('liked'));
    el.classList.add('liked');
    bumpLike(el, +1);
    track('like', { movie_id: movieId, ending_id: endingId });
    if (typeof prev === 'number') {
      const siblings = $$('.ending', card);
      const prevEl = siblings.find(x => Number(x.dataset.endingId) === prev);
      if (prevEl) bumpLike(prevEl, -1);
      postLike(prev, -1);
      track('unlike', { movie_id: movieId, ending_id: prev });
    }
    await idbPut('likes', { movieId, endingId, ts: Date.now() });
    postLike(endingId, +1);
    // Auto-advance to next movie after a small visual delay.
    setTimeout(() => advanceTo(idx + 1), 380);
  }
  function bumpLike(el, delta) {
    const span = $('.likes', el);
    if (!span) return;
    const cur = parseInt(span.textContent.replace(/[k,M.]/g, ''), 10) || 0;
    const next = Math.max(0, cur + delta);
    span.textContent = formatLikes(next);
  }
  function advanceTo(i) {
    const next = state.cardEls[i];
    if (next) next.scrollIntoView({ behavior: 'smooth', block: 'start' });
  }

  // ---------- Active card observer ----------
  function setupObserver() {
    const io = new IntersectionObserver((entries) => {
      for (const ent of entries) {
        if (ent.isIntersecting && ent.intersectionRatio > 0.6) {
          ent.target.classList.add('ready');
          const idx = Number(ent.target.dataset.idx);
          if (idx !== state.activeIdx) {
            state.activeIdx = idx;
            onCardActive(idx);
          }
        }
      }
    }, { threshold: [0, 0.6, 1] });
    state._io = io;
    state.cardEls.forEach(el => io.observe(el));
  }
  function onCardActive(idx) {
    // Emit dwell-time for the card we're leaving.
    if (state.activeIdx >= 0 && state.activeIdx !== idx && state.activeStartedAt) {
      const prev = state.movies[state.activeIdx];
      if (prev) {
        telemetryQueue.push({
          ts: Date.now(),
          type: 'impression',
          movie_id: prev.id,
          duration_ms: Date.now() - state.activeStartedAt,
          audio_playing: state.audioOn ? 1 : 0,
        });
      }
    }
    state.activeStartedAt = Date.now();
    state.cardsViewed += 1;
    maybeShowFeedbackHint();
    maybeShowPremiumNudge();
    const m = state.movies[idx];
    if (!m) return;
    track('card_enter', { movie_id: m.id });
    [idx + 1, idx + 2, idx + 3].forEach(j => preload(state.movies[j]));
    if (m.has_audio) playAudio(m.id, m.audio_version);
    else stopAudio();
    // Infinite scroll: when within 6 cards of the end, shuffle + append.
    if (idx >= state.movies.length - 6) appendShuffledBatch();
  }
  function preload(m) {
    if (!m) return;
    if (m.poster_url) { const im = new Image(); im.src = m.poster_url; }
    if (m.has_audio) {
      // Hint to the SW + browser cache. Version-bust ensures re-encoded
      // tracks don't get served from a stale cache entry.
      fetch(audioUrl(m.id, m.audio_version), { cache: 'force-cache' }).catch(() => {});
    }
  }

  function audioUrl(id, version) {
    const v = (typeof version === 'number' && version > 0) ? `?v=${version}` : '';
    return `/audio/${id}.mp3${v}`;
  }

  // ---------- Local audio ----------
  function ensureAudio() {
    if (state.audio) return state.audio;
    const a = new Audio();
    a.loop = true;
    a.preload = 'auto';
    a.volume = state.audioOn ? 0.65 : 0;
    a.muted = !state.audioOn;
    state.audio = a;
    return a;
  }
  function playAudio(id, version) {
    const a = ensureAudio();
    const src = audioUrl(id, version);
    if (state.currentSrc !== src) {
      state.currentSrc = src;
      a.src = src;
    }
    a.muted = !state.audioOn;
    a.volume = state.audioOn ? 0.65 : 0;
    if (state.audioOn) a.play().catch(() => {});
  }
  function stopAudio() {
    if (state.audio) { try { state.audio.pause(); } catch {} }
    state.currentSrc = null;
  }
  function setAudio(on) {
    state.audioOn = on;
    track(on ? 'audio_on' : 'audio_off');
    const ic = $('#audioIcon');
    if (on) ic.innerHTML = `<path d="M3 10v4h4l5 5V5L7 10H3zm13.5 2c0-1.77-1.02-3.29-2.5-4.03v8.05c1.48-.73 2.5-2.25 2.5-4.02zM14 3.23v2.06c2.89.86 5 3.54 5 6.71s-2.11 5.85-5 6.71v2.06c4.01-.91 7-4.49 7-8.77s-2.99-7.86-7-8.77z"/>`;
    else ic.innerHTML = `<path d="M16.5 12c0-1.77-1.02-3.29-2.5-4.03v2.21l2.45 2.45c.03-.2.05-.41.05-.63zM19 12c0 .94-.2 1.82-.54 2.64l1.51 1.51C20.63 14.91 21 13.5 21 12c0-4.28-2.99-7.86-7-8.77v2.06c2.89.86 5 3.54 5 6.71zM4.27 3L3 4.27 7.73 9H3v6h4l5 5v-6.73l4.25 4.25c-.67.52-1.42.93-2.25 1.18v2.06c1.38-.31 2.63-.95 3.69-1.81L19.73 21 21 19.73l-9-9L4.27 3zM12 4L9.91 6.09 12 8.18V4z"/>`;
    if (state.audio) {
      state.audio.muted = !on;
      state.audio.volume = on ? 0.65 : 0;
      if (on) state.audio.play().catch(() => {});
    }
  }

  // ---------- Splash ----------
  function dismissSplash() {
    const sp = $('#splash');
    sp.classList.add('gone');
    setTimeout(() => sp.remove(), 600);
    setAudio(true);
    const m = state.movies[state.activeIdx >= 0 ? state.activeIdx : 0];
    if (m && m.has_audio) playAudio(m.id, m.audio_version);
  }

  // ---------- Helpers ----------
  function escapeHTML(s) {
    return String(s ?? '').replace(/[&<>"']/g, c => ({
      '&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'
    }[c]));
  }
  function generateGradient(seed) {
    let h = 0;
    for (let i = 0; i < seed.length; i++) h = (h * 31 + seed.charCodeAt(i)) & 0xffff;
    h = h % 360;
    return `linear-gradient(135deg, hsl(${h},60%,18%), hsl(${(h+45)%360},70%,8%) 60%, #000)`;
  }
  function shuffleInPlace(arr) {
    for (let i = arr.length - 1; i > 0; i--) {
      const j = Math.floor(Math.random() * (i + 1));
      [arr[i], arr[j]] = [arr[j], arr[i]];
    }
  }

  // ---------- Feedback widget ----------
  // Subtle icon top-left appears after 3 cards are viewed. Tap → modal with
  // 3 options (interesting / stupid / custom). Submitted feedback POSTs to
  // /api/feedback and then the icon goes dormant for 24 h.
  function maybeShowFeedbackHint() {
    if (state.cardsViewed < 3) return;
    const btn = $('#fbBtn');
    if (!btn || btn.classList.contains('shown')) return;
    if (localStorage.getItem('rw_fb_until') &&
        Date.now() < +localStorage.getItem('rw_fb_until')) return;
    btn.classList.add('shown');
  }
  function openFeedback() {
    track('feedback_open');
    const m = $('#fbModal');
    m.classList.add('open');
    $('#fbCustom').value = '';
  }
  function closeFeedback() {
    $('#fbModal').classList.remove('open');
  }
  async function submitFeedback(kind, text) {
    closeFeedback();
    track('feedback_submit', { extra: { kind, len: (text||'').length } });
    try {
      await fetch('/api/feedback', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ session_id: SESSION_ID, anon_id: ANON_ID, kind, text, mode: 'direct' }),
        keepalive: true,
      });
    } catch {}
    // Hide the icon for 24 h after a submission.
    localStorage.setItem('rw_fb_until', String(Date.now() + 24*60*60*1000));
    $('#fbBtn').classList.remove('shown');
    // Tiny toast.
    const t = document.createElement('div');
    t.className = 'fb-toast';
    t.textContent = 'Thanks ✨';
    document.body.appendChild(t);
    setTimeout(() => t.remove(), 1600);
  }
  function setupFeedback() {
    $('#fbBtn').addEventListener('click', openFeedback);
    $('#fbClose').addEventListener('click', closeFeedback);
    $('#fbModal').addEventListener('click', (e) => {
      if (e.target.id === 'fbModal') closeFeedback();
    });
    $$('#fbModal [data-kind]').forEach(b => {
      b.addEventListener('click', () => {
        const kind = b.dataset.kind;
        if (kind === 'custom') {
          $('#fbCustomWrap').classList.add('open');
          $('#fbCustom').focus();
          return;
        }
        submitFeedback(kind, '');
      });
    });
    $('#fbSubmit').addEventListener('click', () => {
      const txt = $('#fbCustom').value.trim();
      if (!txt) return;
      submitFeedback('custom', txt);
    });
  }

  // ---------- Flag UX (3 reasons + no unflag) ----------
  // Tap the flag icon → bottom sheet with 3 buttons. Once a movie has been
  // flagged we keep the flag in IndexedDB so the next tap shows a toast
  // "Concerns already shared" instead of opening the modal again.
  async function loadFlags() {
    try {
      const all = await idbAll('flags');
      for (const f of all) state.flagMap.set(f.movieId, f);
    } catch {}
  }
  async function onFlagClick(movieId) {
    const existing = state.flagMap.get(movieId);
    if (existing) {
      const labels = {
        wrong_audio: 'wrong song / audio',
        wrong_poster: 'wrong poster',
        other: 'feedback',
      };
      showToast('Concerns already shared (' + (labels[existing.reason] || 'reported') + ') ✨');
      return;
    }
    state.pendingFlagMovie = movieId;
    track('flag_open', { movie_id: movieId });
    $('#flagCustomWrap').classList.remove('open');
    $('#flagCustom').value = '';
    $('#flagModal').classList.add('open');
  }
  function closeFlag() {
    $('#flagModal').classList.remove('open');
    state.pendingFlagMovie = null;
  }
  async function submitFlag(reason, customText) {
    const movieId = state.pendingFlagMovie;
    if (!movieId) return;
    closeFlag();
    track('flag_submit', { movie_id: movieId, extra: { reason, len: (customText||'').length } });
    const ts = Date.now();
    state.flagMap.set(movieId, { movieId, reason, customText: customText || '', ts });
    try {
      await idbPut('flags', { movieId, reason, customText: customText || '', ts });
    } catch {}
    // Mark the visible flag icon as flagged.
    $$(`.card[data-movie="${cssEscape(movieId)}"] .flag-btn`).forEach(el => el.classList.add('flagged'));
    try {
      await fetch('/api/flag', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          session_id: SESSION_ID,
          anon_id: ANON_ID,
          mode: 'direct',
          movie_id: movieId,
          reason,
          custom_text: customText || '',
        }),
        keepalive: true,
      });
    } catch {}
    showToast('Thanks — flag sent ✨');
  }
  function setupFlag() {
    $('#flagModal').addEventListener('click', (e) => {
      if (e.target.id === 'flagModal') closeFlag();
    });
    $$('#flagModal [data-reason]').forEach(b => {
      b.addEventListener('click', () => {
        const reason = b.dataset.reason;
        if (reason === 'other') {
          $('#flagCustomWrap').classList.add('open');
          $('#flagCustom').focus();
          return;
        }
        submitFlag(reason, '');
      });
    });
    $('#flagSubmit').addEventListener('click', () => {
      const txt = $('#flagCustom').value.trim();
      submitFlag('other', txt);
    });
  }

  // ---------- Community endings ----------
  // The /api/movies payload includes m.community: an array (cap 25) of
  // {id, author, text, likes, avg_rating, rating_n}. We render in the
  // sheet on demand; not inline by default to keep the scroll surface
  // distraction-free (per rubber-duck #6 + #7).
  function openCommunitySheet(m) {
    state.communityMovie = m;
    track('community_open', { movie_id: m.id });
    const list = $('#communityList');
    list.innerHTML = '';
    const items = (m.community || []).slice().sort((a,b)=>(b.likes||0)-(a.likes||0) || (b.rating_n||0)-(a.rating_n||0));
    if (!items.length) {
      const empty = document.createElement('div');
      empty.className = 'empty';
      empty.textContent = 'No community endings yet — be the first ✨';
      list.appendChild(empty);
    } else {
      for (const ce of items) list.appendChild(buildCommunityItem(ce));
    }
    $('#communityTitle').textContent = `Community endings — ${escapeHTML(m.title)}`;
    $('#communityText').value = '';
    $('#communityAuthor').value = localStorage.getItem('rw_handle') || '';
    $('#communitySubmit').disabled = false;
    $('#communitySheet').classList.add('open');
  }
  function closeCommunitySheet() {
    $('#communitySheet').classList.remove('open');
    state.communityMovie = null;
  }
  function buildCommunityItem(ce) {
    const wrap = document.createElement('div');
    wrap.className = 'ce-item';
    wrap.dataset.endingId = ce.id;
    const myLiked = state.communityLikes.has(ce.id);
    const myRating = state.communityRatings.get(ce.id) || 0;
    const avg = ce.avg_rating || 0;
    const stars = [1,2,3,4,5].map(n => {
      const filled = (myRating ? n <= myRating : n <= Math.round(avg));
      return `<span data-n="${n}" class="${filled ? 'filled' : ''}">★</span>`;
    }).join('');
    wrap.innerHTML = `
      <div class="ce-text">${escapeHTML(ce.text)}</div>
      <div class="ce-meta">
        <span class="author">@${escapeHTML(ce.author || 'anonymous')}</span>
        <span class="heart-mini ${myLiked ? 'liked' : ''}" data-act="like">❤ ${formatLikes(ce.likes || 0)}</span>
        <span class="rate-info">${ce.rating_n ? `${avg.toFixed(1)}★ · ${ce.rating_n}` : 'rate'}</span>
        <span class="stars" data-act="rate">${stars}</span>
      </div>
    `;
    wrap.querySelector('[data-act="like"]').addEventListener('click', () => communityLike(ce, wrap));
    wrap.querySelectorAll('.stars span').forEach(s => {
      s.addEventListener('click', () => communityRate(ce, Number(s.dataset.n), wrap));
    });
    return wrap;
  }
  async function communityLike(ce, wrap) {
    const wasLiked = state.communityLikes.has(ce.id);
    const delta = wasLiked ? -1 : +1;
    if (wasLiked) state.communityLikes.delete(ce.id);
    else state.communityLikes.add(ce.id);
    persistCommunityLikes();
    ce.likes = Math.max(0, (ce.likes || 0) + delta);
    const btn = wrap.querySelector('[data-act="like"]');
    btn.classList.toggle('liked', !wasLiked);
    btn.innerHTML = `❤ ${formatLikes(ce.likes)}`;
    track('community_like', { movie_id: state.communityMovie?.id, extra: { ending_id: ce.id, delta } });
    try {
      await fetch('/api/community/like-ending', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ ending_id: ce.id, delta }),
      });
    } catch {}
  }
  async function communityRate(ce, rating, wrap) {
    state.communityRatings.set(ce.id, rating);
    persistCommunityRatings();
    wrap.querySelectorAll('.stars span').forEach(s => {
      s.classList.toggle('filled', Number(s.dataset.n) <= rating);
    });
    track('community_rate', { movie_id: state.communityMovie?.id, extra: { ending_id: ce.id, rating } });
    try {
      const r = await fetch('/api/community/rate-ending', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ ending_id: ce.id, rating, anon_id: ANON_ID }),
      });
      if (r.ok) {
        const j = await r.json();
        ce.avg_rating = j.avg_rating; ce.rating_n = j.rating_n;
        const info = wrap.querySelector('.rate-info');
        if (info) info.textContent = j.rating_n ? `${(j.avg_rating||0).toFixed(1)}★ · ${j.rating_n}` : 'rate';
      }
    } catch {}
  }
  async function communitySubmit() {
    const m = state.communityMovie;
    if (!m) return;
    const text = $('#communityText').value.trim();
    const author = $('#communityAuthor').value.trim();
    if (text.length < 4) { showToast('Need at least 4 characters'); return; }
    $('#communitySubmit').disabled = true;
    if (author) localStorage.setItem('rw_handle', author);
    track('community_submit', { movie_id: m.id, extra: { len: text.length } });
    try {
      const r = await fetch('/api/community/submit-ending', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ content_id: m.id, text, author, anon_id: ANON_ID }),
      });
      if (!r.ok) {
        const t = await r.text();
        showToast(t || 'Could not post — try again');
        $('#communitySubmit').disabled = false;
        return;
      }
      const j = await r.json();
      // Optimistically prepend and re-render.
      m.community = m.community || [];
      m.community.unshift({
        id: j.ending_id, author: j.author, text: j.text,
        likes: 0, avg_rating: 0, rating_n: 0,
      });
      // Re-render the list area inside the open sheet.
      openCommunitySheet(m);
      // Update the FAB badge on the card.
      $$(`.card[data-movie="${cssEscape(m.id)}"] .community-fab`).forEach(p => {
        const c = m.community.length;
        const existingBadge = p.querySelector('.badge');
        const txt = c > 99 ? '99+' : String(c);
        if (existingBadge) existingBadge.textContent = txt;
        else if (c > 0) p.insertAdjacentHTML('beforeend', `<span class="badge">${txt}</span>`);
      });
      showToast('Posted ✨');
    } catch (e) {
      showToast('Network error');
      $('#communitySubmit').disabled = false;
    }
  }
  function setupCommunity() {
    $('#communityClose').addEventListener('click', closeCommunitySheet);
    $('#communitySheet').addEventListener('click', (e) => {
      if (e.target.id === 'communitySheet') closeCommunitySheet();
    });
    $('#communitySubmit').addEventListener('click', communitySubmit);
    // Hydrate community-likes / ratings from localStorage.
    try {
      const ls = JSON.parse(localStorage.getItem('rw_clikes') || '[]');
      ls.forEach(id => state.communityLikes.add(id));
    } catch {}
    try {
      const lr = JSON.parse(localStorage.getItem('rw_crates') || '{}');
      Object.entries(lr).forEach(([k,v]) => state.communityRatings.set(Number(k), Number(v)));
    } catch {}
  }
  function persistCommunityLikes() {
    localStorage.setItem('rw_clikes', JSON.stringify(Array.from(state.communityLikes)));
  }
  function persistCommunityRatings() {
    const o = {}; state.communityRatings.forEach((v,k) => { o[k] = v; });
    localStorage.setItem('rw_crates', JSON.stringify(o));
  }

  // ---------- Premium nudge ----------
  // v1.3: full-modal sales pitch (instead of v1.2 corner banner that
  // covered the bottom ending). Shown after 8 cards first time, then
  // every 25 cards. Dismissible with explicit "Maybe later".
  function maybeShowPremiumNudge() {
    if (localStorage.getItem('rw_premium_token')) return;  // already paid
    const NUDGE_FIRST = 8;
    const NUDGE_REPEAT = 25;
    const dismissed = +(localStorage.getItem('rw_nudge_dismissed') || 0);
    const cardsAtDismiss = +(localStorage.getItem('rw_nudge_cards') || 0);
    const enough = state.cardsViewed >= NUDGE_FIRST &&
      (!dismissed || state.cardsViewed - cardsAtDismiss >= NUDGE_REPEAT);
    if (!enough) return;
    const el = $('#premiumNudge');
    if (!el || el.classList.contains('show')) return;
    el.classList.add('show');
    document.body.classList.add('nudge-open');
    track('premium_nudge_shown', { extra: { cards: state.cardsViewed } });
  }
  function setupPremiumNudge() {
    $('#premiumGo').addEventListener('click', () => {
      track('premium_buy_click');
      window.location.href = '/buy';
    });
    const closeNudge = () => {
      const el = $('#premiumNudge');
      el.classList.remove('show');
      document.body.classList.remove('nudge-open');
      localStorage.setItem('rw_nudge_dismissed', String(Date.now()));
      localStorage.setItem('rw_nudge_cards', String(state.cardsViewed));
      track('premium_nudge_dismissed');
    };
    $('#premiumClose').addEventListener('click', closeNudge);
    $('#premiumLater')?.addEventListener('click', closeNudge);
    // Tap on backdrop closes too.
    $('#premiumNudge').addEventListener('click', (e) => {
      if (e.target.id === 'premiumNudge') closeNudge();
    });
  }

  function showToast(text) {
    const t = document.createElement('div');
    t.className = 'fb-toast';
    t.textContent = text;
    document.body.appendChild(t);
    setTimeout(() => t.remove(), 2200);
  }
  function cssEscape(s) {
    if (window.CSS && CSS.escape) return CSS.escape(s);
    return String(s).replace(/([^a-zA-Z0-9_-])/g, '\\$1');
  }

  // ---------- Boot ----------
  $('#splash').addEventListener('click', dismissSplash, { once: true });
  $('#audioBtn').addEventListener('click', () => setAudio(!state.audioOn));
  setupFeedback();
  setupFlag();
  setupPremiumNudge();
  setupCommunity();
  loadFlags();

  (async function boot() {
    try {
      await loadLikes();
      await fetchMovies();
    } catch (e) {
      console.error('boot failed', e);
      $('#empty').classList.add('show');
      $('#empty').querySelector('p').textContent = 'Could not load movies. Pull to retry.';
    }
  })();

  // Stream new endings + like counts every 60 s. Don't reshuffle on refresh
  // — just merge fresh likes into the existing in-place card list.
  setInterval(async () => {
    try {
      const r = await fetch('/api/movies', { cache: 'no-store' });
      if (!r.ok) return;
      const data = await r.json();
      await idbPut('cache', { k: 'movies', v: data, t: Date.now() });
      const byId = new Map(data.movies.map(m => [m.id, m]));
      state.movies.forEach(m => {
        const fresh = byId.get(m.id);
        if (!fresh) return;
        m.endings = fresh.endings;
        m.has_audio = fresh.has_audio;
        m.audio_version = fresh.audio_version;
        m.poster_url = m.poster_url || fresh.poster_url;
        m.views = fresh.views;
        m.likes_total = fresh.likes_total;
        m.conversion_pct = fresh.conversion_pct;
        m.community = fresh.community || [];
      });
      state.cardEls.forEach(card => {
        const m = state.movies[Number(card.dataset.idx)];
        if (!m) return;
        const liked = state.likeMap.get(m.id);
        $$('.ending', card).forEach((el, i) => {
          const e = m.endings[i];
          if (!e || el.classList.contains('placeholder')) return;
          const span = $('.likes', el);
          if (span) span.textContent = formatLikes(e.likes || 0);
          el.dataset.endingId = e.id;
          el.classList.toggle('liked', liked === e.id);
        });
        // Update stats badge (v1.3 bigger format with <b>)
        const stats = $('.top .stats', card);
        if (stats) {
          const conv = m.conversion_pct || 0;
          stats.innerHTML = `
            <span><em>👁</em><b>${formatLikes(m.views||0)}</b></span>
            <span><em>❤</em><b>${formatLikes(m.likes_total||0)}</b></span>
            ${conv > 0 ? `<span><em>⚡</em><b>${conv}%</b></span>` : ''}
          `;
        }
        // Update community FAB badge
        const fab = $('.community-fab', card);
        if (fab) {
          const c = (m.community || []).length;
          const existingBadge = fab.querySelector('.badge');
          if (c > 0) {
            const txt = c > 99 ? '99+' : String(c);
            if (existingBadge) existingBadge.textContent = txt;
            else fab.insertAdjacentHTML('beforeend', `<span class="badge">${txt}</span>`);
          } else if (existingBadge) {
            existingBadge.remove();
          }
        }
      });
    } catch {}
  }, 60_000);
})();
