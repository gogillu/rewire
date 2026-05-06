// Rewire /sagar build — vanilla JS.
// Differences from the direct (/) build:
//   * Mode-tagged telemetry / feedback (so dashboards split / vs /sagar).
//   * Hits /api/sagar/movies which returns per-movie stats: views,
//     likes_total, conversion_pct, song_query.
//   * Renders a stats pill under the title (👁 / ❤️ / ⚡).
//   * 🚫 flag-audio button → POST /api/sagar/flag-audio (writes to feedbacks
//     with kind='wrong-audio').
//   * 📊 leaderboard overlay — full ranked list, sortable by likes/views/⚡.
//   * Long-press / info-tap on the audio button shows the song search query
//     so Sagar (and anyone else) can see "wo gaana kahan se le rha hai".
//   * Separate IndexedDB ('rewire-sagar') so likes don't bleed across builds.
(function () {
  'use strict';

  const MODE = 'sagar';
  const API = {
    movies: '/api/sagar/movies',
    leaderboard: '/api/sagar/leaderboard',
    flagAudio: '/api/sagar/flag-audio',
    like: '/api/like',
    events: '/api/events',
    feedback: '/api/feedback',
  };

  const $  = (sel, root = document) => root.querySelector(sel);
  const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

  // ---------- IndexedDB ----------
  const DB_NAME = 'rewire-sagar';
  const DB_VER  = 1;
  function openDB() {
    return new Promise((resolve, reject) => {
      const req = indexedDB.open(DB_NAME, DB_VER);
      req.onupgradeneeded = () => {
        const db = req.result;
        if (!db.objectStoreNames.contains('likes')) {
          db.createObjectStore('likes', { keyPath: 'movieId' });
        }
        if (!db.objectStoreNames.contains('cache')) {
          db.createObjectStore('cache', { keyPath: 'k' });
        }
        if (!db.objectStoreNames.contains('flags')) {
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
    movies: [],            // shuffled order, each item carries stats
    poolSrc: [],           // source pool (deterministic order from server)
    cardEls: [],
    likeMap: new Map(),    // movieId -> endingId
    flagSet: new Set(),    // already-flagged movie ids (this device)
    audioOn: false,
    audio: null,
    activeIdx: -1,
    currentSrc: null,
    activeStartedAt: 0,
    cardsViewed: 0,
  };

  // ---------- Telemetry ----------
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
      session_id: SESSION_ID, anon_id: ANON_ID, mode: MODE,
      os: ENV.os, browser: ENV.browser, device: ENV.device,
      screen_w: window.innerWidth, screen_h: window.innerHeight,
      events: batch,
    });
    try {
      if (useBeacon && navigator.sendBeacon) {
        navigator.sendBeacon(API.events, new Blob([body], { type: 'application/json' }));
      } else {
        await fetch(API.events, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body, keepalive: true,
        });
      }
    } catch {} finally { flushing = false; }
  }
  setInterval(() => flushTelemetry(false), 5000);
  window.addEventListener('pagehide', () => {
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
  track('session_start', { extra: { ref: document.referrer || '', tz: Intl.DateTimeFormat().resolvedOptions().timeZone || '', mode: MODE } });

  // ---------- API ----------
  async function fetchMovies() {
    const cached = await idbGet('cache', 'movies');
    if (cached && Date.now() - cached.t < 1000 * 60 * 30) {
      applyMoviePayload(cached.v);
      refresh().catch(() => {});
      return;
    }
    await refresh();
  }
  async function refresh() {
    const r = await fetch(API.movies, { cache: 'no-store' });
    if (!r.ok) throw new Error('fetch movies failed');
    const data = await r.json();
    await idbPut('cache', { k: 'movies', v: data, t: Date.now() });
    applyMoviePayload(data);
  }
  function applyMoviePayload(payload) {
    const arr = (payload.movies || []).slice();
    shuffleInPlace(arr);
    state.poolSrc = (payload.movies || []).slice();
    state.movies = arr;
    render();
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
    if (state._io) state.cardEls.slice(offset).forEach(el => state._io.observe(el));
  }
  async function postLike(endingId, delta) {
    try {
      const r = await fetch(API.like, {
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
    const flags = await idbAll('flags');
    for (const row of flags) state.flagSet.add(row.movieId);
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

    // TOP — title, year/rating, stats pill
    const top = document.createElement('div');
    top.className = 'top';
    top.innerHTML = `
      <div class="title">${escapeHTML(m.title)}</div>
      <div class="sub">${m.year || ''} · ★ ${(m.imdb_rating || 0).toFixed(1)}</div>
      <button class="stats-pill" data-role="stats">
        <span><span class="num">${formatLikes(m.views || 0)}</span> <span class="lbl">👁 views</span></span>
        <span class="sep">·</span>
        <span><span class="num">${formatLikes(m.likes_total || 0)}</span> <span class="lbl">❤️ likes</span></span>
        <span class="sep">·</span>
        <span><span class="num">⚡ ${(m.conversion_pct || 0).toFixed(1)}%</span></span>
      </button>
    `;
    card.appendChild(top);

    // BOTTOM — three endings
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

    // VERY BOTTOM — Rewire brand chip + sagar build chip
    const brand = document.createElement('div');
    brand.className = 'brand';
    brand.innerHTML = `<span class="logo">Rewire</span><span class="build">Sagar</span>`;
    card.appendChild(brand);
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
      advanceTo(idx + 1);
      return;
    }
    state.likeMap.set(movieId, endingId);
    $$('.ending', card).forEach(x => x.classList.remove('liked'));
    el.classList.add('liked');
    bumpLike(el, +1);
    bumpStats(card, +1, prev !== undefined ? 0 : +1); // net likes_total +1 only on first like
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
    setTimeout(() => advanceTo(idx + 1), 380);
  }
  function bumpLike(el, delta) {
    const span = $('.likes', el);
    if (!span) return;
    const cur = parseInt(span.textContent.replace(/[k,M.]/g, ''), 10) || 0;
    const next = Math.max(0, cur + delta);
    span.textContent = formatLikes(next);
  }
  // Optimistically update the stats-pill on the card after a like.
  function bumpStats(card, deltaPerEnding, deltaTotal) {
    const m = state.movies[Number(card.dataset.idx)];
    if (!m) return;
    if (deltaTotal !== 0) {
      m.likes_total = Math.max(0, (m.likes_total || 0) + deltaTotal);
      const nums = $$('.stats-pill .num', card);
      if (nums[1]) nums[1].textContent = formatLikes(m.likes_total);
      if (m.views > 0 && nums[2]) {
        const conv = m.likes_total / m.views * 100;
        m.conversion_pct = conv;
        nums[2].textContent = '⚡ ' + conv.toFixed(1) + '%';
      }
    }
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
    const m = state.movies[idx];
    if (!m) return;
    track('card_enter', { movie_id: m.id });
    [idx + 1, idx + 2, idx + 3].forEach(j => preload(state.movies[j]));
    if (m.has_audio) playAudio(m.id, m.audio_version);
    else stopAudio();
    // Reflect flag state on the audio cluster.
    $('#flagBtn').classList.toggle('flagged', state.flagSet.has(m.id));
    // Update song-info popover if currently open.
    if ($('#songInfo').classList.contains('shown')) {
      $('#songQuery').textContent = m.song_query || '(unknown — no metadata)';
    }
    if (idx >= state.movies.length - 6) appendShuffledBatch();
  }
  function preload(m) {
    if (!m) return;
    if (m.poster_url) { const im = new Image(); im.src = m.poster_url; }
    if (m.has_audio) {
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
  function showToast(text, ms = 1600) {
    const t = document.createElement('div');
    t.className = 'toast';
    t.textContent = text;
    document.body.appendChild(t);
    setTimeout(() => t.remove(), ms);
  }

  // ---------- Flag wrong audio ----------
  // Tap on the 🚫 button → POST /api/sagar/flag-audio for current movie.
  // Long-press → show song-source popover so Sagar can see "wo gaana kahan
  // se le rha hai" before deciding to flag.
  async function flagCurrentAudio() {
    const m = state.movies[state.activeIdx];
    if (!m) { showToast('No active movie'); return; }
    if (state.flagSet.has(m.id)) {
      showToast('Already flagged for ' + m.title);
      return;
    }
    state.flagSet.add(m.id);
    $('#flagBtn').classList.add('flagged');
    track('flag_audio', { movie_id: m.id, extra: { song_query: m.song_query || '' } });
    try {
      await fetch(API.flagAudio, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          session_id: SESSION_ID, anon_id: ANON_ID,
          movie_id: m.id, note: 'wrong music',
        }),
        keepalive: true,
      });
    } catch {}
    await idbPut('flags', { movieId: m.id, ts: Date.now(), title: m.title });
    showToast('Flagged: ' + m.title + ' 🚫');
  }
  function showSongInfo() {
    const m = state.movies[state.activeIdx];
    const q = (m && m.song_query) ? m.song_query : '(no metadata)';
    $('#songQuery').textContent = q;
    $('#songInfo').classList.add('shown');
    setTimeout(() => $('#songInfo').classList.remove('shown'), 4000);
  }

  // ---------- Leaderboard ----------
  let lbCurrentSort = 'likes';
  async function openLeaderboard() {
    track('leaderboard_open');
    $('#lbOverlay').classList.add('open');
    await loadLeaderboard(lbCurrentSort);
  }
  function closeLeaderboard() {
    $('#lbOverlay').classList.remove('open');
  }
  async function loadLeaderboard(sortBy) {
    lbCurrentSort = sortBy;
    $$('#lbSorts button').forEach(b => b.classList.toggle('active', b.dataset.sort === sortBy));
    const list = $('#lbList');
    list.innerHTML = '<div class="lb-empty">Loading…</div>';
    try {
      const r = await fetch(API.leaderboard + '?sort=' + encodeURIComponent(sortBy), { cache: 'no-store' });
      if (!r.ok) throw new Error('lb fetch failed');
      const data = await r.json();
      renderLeaderboard(data.leaderboard || []);
    } catch (e) {
      list.innerHTML = '<div class="lb-empty">Could not load leaderboard. Try again later.</div>';
    }
  }
  function renderLeaderboard(rows) {
    const list = $('#lbList');
    if (!rows.length) {
      list.innerHTML = '<div class="lb-empty">No data yet — be the first to interact!</div>';
      return;
    }
    list.innerHTML = '';
    const frag = document.createDocumentFragment();
    rows.forEach(r => {
      const row = document.createElement('div');
      row.className = 'lb-row';
      if (r.rank === 1) row.classList.add('top1');
      else if (r.rank === 2) row.classList.add('top2');
      else if (r.rank === 3) row.classList.add('top3');
      const poster = r.poster_url ? `style="background-image:url('${r.poster_url}')"` : '';
      row.innerHTML = `
        <div class="rank">#${r.rank}</div>
        <div class="poster" ${poster}></div>
        <div class="meta">
          <div class="name">${escapeHTML(r.title)} <small style="opacity:.5;font-weight:400">(${r.year || ''})</small></div>
          <div class="stats">
            <b>❤️ ${formatLikes(r.likes_total)}</b> ·
            <b>👁 ${formatLikes(r.views)}</b> ·
            <b>⚡ ${(r.conversion_pct || 0).toFixed(1)}%</b>
          </div>
        </div>
      `;
      row.addEventListener('click', () => {
        track('leaderboard_jump', { movie_id: r.id });
        // Scroll to first card matching this id.
        const targetIdx = state.movies.findIndex(m => m.id === r.id);
        if (targetIdx >= 0) {
          closeLeaderboard();
          state.cardEls[targetIdx]?.scrollIntoView({ behavior: 'smooth', block: 'start' });
        }
      });
      frag.appendChild(row);
    });
    list.appendChild(frag);
  }

  // ---------- Feedback widget ----------
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
  function closeFeedback() { $('#fbModal').classList.remove('open'); }
  async function submitFeedback(kind, text) {
    closeFeedback();
    track('feedback_submit', { extra: { kind, len: (text||'').length } });
    try {
      await fetch(API.feedback, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ session_id: SESSION_ID, anon_id: ANON_ID, kind, text, mode: MODE }),
        keepalive: true,
      });
    } catch {}
    localStorage.setItem('rw_fb_until', String(Date.now() + 24*60*60*1000));
    $('#fbBtn').classList.remove('shown');
    showToast('Thanks ✨');
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

  // ---------- Boot ----------
  $('#splash').addEventListener('click', dismissSplash, { once: true });
  $('#audioBtn').addEventListener('click', () => setAudio(!state.audioOn));
  // Long-press on audio button → show song-source. Plain tap is sound-toggle.
  let audioPressTimer = null;
  $('#audioBtn').addEventListener('pointerdown', () => {
    audioPressTimer = setTimeout(() => { showSongInfo(); audioPressTimer = null; }, 500);
  });
  $('#audioBtn').addEventListener('pointerup', () => {
    if (audioPressTimer) { clearTimeout(audioPressTimer); audioPressTimer = null; }
  });
  $('#audioBtn').addEventListener('pointercancel', () => {
    if (audioPressTimer) { clearTimeout(audioPressTimer); audioPressTimer = null; }
  });
  $('#flagBtn').addEventListener('click', flagCurrentAudio);
  $('#lbBtn').addEventListener('click', openLeaderboard);
  $('#lbClose').addEventListener('click', closeLeaderboard);
  $$('#lbSorts button').forEach(b => {
    b.addEventListener('click', () => loadLeaderboard(b.dataset.sort));
  });
  // Tap on stats-pill anywhere → also opens leaderboard so anyone confused
  // by the numbers can see how the ranking is computed.
  document.addEventListener('click', (ev) => {
    if (ev.target.closest('[data-role="stats"]')) {
      ev.preventDefault();
      openLeaderboard();
    }
  });
  setupFeedback();

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

  // Refresh stats every 60s — merge fresh likes/views into existing cards.
  setInterval(async () => {
    try {
      const r = await fetch(API.movies, { cache: 'no-store' });
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
        m.song_query = fresh.song_query;
        m.views = fresh.views;
        m.likes_total = fresh.likes_total;
        m.conversion_pct = fresh.conversion_pct;
        m.poster_url = m.poster_url || fresh.poster_url;
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
        const nums = $$('.stats-pill .num', card);
        if (nums[0]) nums[0].textContent = formatLikes(m.views || 0);
        if (nums[1]) nums[1].textContent = formatLikes(m.likes_total || 0);
        if (nums[2]) nums[2].textContent = '⚡ ' + (m.conversion_pct || 0).toFixed(1) + '%';
      });
    } catch {}
  }, 60_000);
})();
