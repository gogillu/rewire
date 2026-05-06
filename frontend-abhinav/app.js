// Rewire frontend (Abhinav build) — vanilla JS, hits /api/abhinav/* and
// includes mode='abhinav' on every telemetry / feedback POST so the
// dashboard can split metrics from the main deck.
(function () {
  'use strict';

  const MODE = 'abhinav';
  const API = {
    movies: '/api/abhinav/movies',
    like: '/api/abhinav/like',
    submit: '/api/abhinav/submit-ending',
    rate: '/api/abhinav/rate-ending',
    events: '/api/events',
    feedback: '/api/feedback',
  };

  const $  = (sel, root = document) => root.querySelector(sel);
  const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

  // ---------- IndexedDB ----------
  const DB_NAME = 'rewire-abhinav';
  const DB_VER  = 1;
  function openDB() {
    return new Promise((resolve, reject) => {
      const req = indexedDB.open(DB_NAME, DB_VER);
      req.onupgradeneeded = () => {
        const db = req.result;
        if (!db.objectStoreNames.contains('likes')) {
          db.createObjectStore('likes', { keyPath: 'movieId' });
        }
        if (!db.objectStoreNames.contains('ratings')) {
          db.createObjectStore('ratings', { keyPath: 'endingId' });
        }
        if (!db.objectStoreNames.contains('cache')) {
          db.createObjectStore('cache', { keyPath: 'k' });
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
    items: [],            // shuffled order of contents (movies + series)
    cardEls: [],
    likeMap: new Map(),   // contentId -> { ending_id, target }
    ratingMap: new Map(), // endingId -> rating(1..5)
    audioOn: false,
    audio: null,
    activeIdx: -1,
    currentSrc: null,
    activeStartedAt: 0,
    cardsViewed: 0,
    weTarget: null,       // {contentId, title, idx}
  };

  // ---------- Telemetry ----------
  const SESSION_ID = (() => {
    let s = sessionStorage.getItem('rwa_sid');
    if (!s) { s = uuid(); sessionStorage.setItem('rwa_sid', s); }
    return s;
  })();
  const ANON_ID = (() => {
    // Reuse the main-app anon_id if present so we don't double-count one
    // person across the two builds. New visitors get their own.
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
      const m = state.items[state.activeIdx];
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
  track('session_start', { extra: { ref: document.referrer || '', tz: Intl.DateTimeFormat().resolvedOptions().timeZone || '', build: 'abhinav' } });

  // ---------- API ----------
  async function fetchContent() {
    const cached = await idbGet('cache', 'content');
    if (cached && Date.now() - cached.t < 1000 * 60 * 30) {
      applyPayload(cached.v);
      refresh().catch(() => {});
      return;
    }
    await refresh();
  }
  async function refresh() {
    const r = await fetch(API.movies, { cache: 'no-store' });
    if (!r.ok) throw new Error('fetch content failed');
    const data = await r.json();
    await idbPut('cache', { k: 'content', v: data, t: Date.now() });
    applyPayload(data);
  }
  function applyPayload(payload) {
    const arr = (payload.content || []).slice();
    shuffleInPlace(arr);
    state.poolSrc = (payload.content || []).slice();
    state.items = arr;
    render();
    for (let i = 0; i < 5; i++) preload(arr[i]);
  }
  function appendShuffledBatch() {
    if (!state.poolSrc || !state.poolSrc.length) return;
    const batch = state.poolSrc.slice();
    shuffleInPlace(batch);
    const offset = state.items.length;
    state.items.push(...batch);
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
  async function postLike(target, endingId, delta) {
    try {
      const r = await fetch(API.like, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ ending_id: endingId, target, delta }),
      });
      if (!r.ok) return null;
      return await r.json();
    } catch { return null; }
  }
  async function postSubmitEnding(contentId, text, author) {
    const r = await fetch(API.submit, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ content_id: contentId, text, author, anon_id: ANON_ID }),
    });
    if (!r.ok) return null;
    return await r.json();
  }
  async function postRate(endingId, target, rating) {
    const r = await fetch(API.rate, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ ending_id: endingId, target, rating, anon_id: ANON_ID }),
    });
    if (!r.ok) return null;
    return await r.json();
  }

  // ---------- Likes / ratings persistence ----------
  async function loadLikes() {
    const all = await idbAll('likes');
    for (const row of all) state.likeMap.set(row.movieId, row);
    const rs = await idbAll('ratings');
    for (const row of rs) state.ratingMap.set(row.endingId, row.rating);
  }

  // ---------- Render ----------
  function render() {
    const deck = $('#deck');
    deck.innerHTML = '';
    state.cardEls = [];
    if (!state.items.length) {
      $('#empty').classList.add('show');
      return;
    }
    $('#empty').classList.remove('show');
    const frag = document.createDocumentFragment();
    state.items.forEach((m, i) => {
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

    // TOP — title + year/rating + kind chip
    const top = document.createElement('div');
    top.className = 'top';
    const kindLabel = m.kind === 'series' ? 'TV Series' : 'Movie';
    top.innerHTML = `
      <div class="title">${escapeHTML(m.title)}</div>
      <div class="sub">${m.year || ''} · ★ ${(m.imdb_rating || 0).toFixed(1)}</div>
      <div class="kind kind-${m.kind === 'series' ? 'series' : 'movie'}">${kindLabel}</div>
    `;
    card.appendChild(top);

    // BOTTOM — endings + write-your-own tile
    const wrap = document.createElement('div');
    wrap.className = 'endings';
    const liked = state.likeMap.get(m.id);
    const endings = (m.endings || []).slice(0, 3);
    // Pad with placeholders to keep layout stable.
    while (endings.length < 3) endings.push(null);
    endings.forEach((e) => {
      const el = document.createElement('div');
      if (!e) {
        el.className = 'ending placeholder';
        el.innerHTML = `<div class="body"><div class="text">Rewriting reality…</div></div>`;
        wrap.appendChild(el);
        return;
      }
      el.className = 'ending';
      if (e.source === 'community') el.classList.add('community');
      if (liked && liked.ending_id === e.id && liked.target === e.target) {
        el.classList.add('liked');
      }
      el.dataset.endingId = e.id;
      el.dataset.target = e.target;

      const textWords = String(e.text || '').trim().split(/\s+/).length;
      const longish = textWords > 12 || (e.text || '').length > 70;
      const author = e.author || (e.source === 'community' ? 'anonymous' : 'AI · Canon');
      const ratingStars = renderStars(e.id, e.target, state.ratingMap.get(e.id));

      el.innerHTML = `
        <div class="body">
          <div class="text">${escapeHTML(e.text)}</div>
          <div class="meta">
            <span class="author">by ${escapeHTML(author)}</span>
            ${longish ? `<button class="more-btn" type="button">read more</button>` : ''}
            ${e.source === 'community' ? `<span class="stars" data-eid="${e.id}" data-target="${e.target}">${ratingStars}</span>` : ''}
          </div>
        </div>
        <div class="row">
          <i class="heart"></i><span class="likes">${formatLikes(e.likes || 0)}</span>
        </div>
      `;
      // "read more" toggle — must not bubble into the like handler.
      const moreBtn = el.querySelector('.more-btn');
      if (moreBtn) {
        moreBtn.addEventListener('click', (ev) => {
          ev.stopPropagation(); ev.preventDefault();
          el.classList.toggle('expanded');
          moreBtn.textContent = el.classList.contains('expanded') ? 'read less' : 'read more';
          track('read_more', { movie_id: m.id, ending_id: e.id });
        });
      }
      // Star ratings (community only).
      const starWrap = el.querySelector('.stars');
      if (starWrap) {
        starWrap.querySelectorAll('button').forEach(btn => {
          btn.addEventListener('click', (ev) => {
            ev.stopPropagation(); ev.preventDefault();
            const r = +btn.dataset.r;
            applyRating(starWrap, e.id, e.target, r);
          });
        });
      }
      el.addEventListener('click', (ev) => {
        ev.preventDefault();
        onLike(m.id, e, el, card, idx);
      });
      wrap.appendChild(el);
    });

    // 4th tile — write your own.
    const own = document.createElement('div');
    own.className = 'ending write-own';
    own.innerHTML = `<div class="body"><div class="text">＋ Write your own ending</div></div>`;
    own.addEventListener('click', (ev) => {
      ev.preventDefault();
      openWriteOwn(m, idx);
    });
    wrap.appendChild(own);

    card.appendChild(wrap);

    const brand = document.createElement('div');
    brand.className = 'brand';
    brand.innerHTML = `<div class="logo">Rewire</div>`;
    card.appendChild(brand);
    return card;
  }

  function renderStars(eid, target, mine) {
    let s = '';
    for (let i = 1; i <= 5; i++) {
      const lit = mine && mine >= i ? 'lit' : '';
      s += `<button class="${lit}" data-r="${i}" data-eid="${eid}" data-target="${target}" aria-label="Rate ${i}">★</button>`;
    }
    return s;
  }
  async function applyRating(wrap, endingId, target, rating) {
    state.ratingMap.set(endingId, rating);
    wrap.querySelectorAll('button').forEach(b => {
      b.classList.toggle('lit', +b.dataset.r <= rating);
    });
    track('rate_ending', { movie_id: '', ending_id: endingId, extra: { rating } });
    await idbPut('ratings', { endingId, rating, ts: Date.now() });
    await postRate(endingId, target, rating);
  }

  function formatLikes(n) {
    if (n < 1000) return String(n);
    if (n < 1_000_000) return (n / 1000).toFixed(n < 10_000 ? 1 : 0) + 'k';
    return (n / 1_000_000).toFixed(1) + 'M';
  }

  // ---------- Like ----------
  async function onLike(movieId, ending, el, card, idx) {
    const prev = state.likeMap.get(movieId);
    if (prev && prev.ending_id === ending.id) {
      advanceTo(idx + 1);
      return;
    }
    state.likeMap.set(movieId, { ending_id: ending.id, target: ending.target });
    $$('.ending', card).forEach(x => x.classList.remove('liked'));
    el.classList.add('liked');
    bumpLike(el, +1);
    track('like', { movie_id: movieId, ending_id: ending.id });
    if (prev) {
      const siblings = $$('.ending', card);
      const prevEl = siblings.find(x => Number(x.dataset.endingId) === prev.ending_id);
      if (prevEl) bumpLike(prevEl, -1);
      postLike(prev.target, prev.ending_id, -1);
      track('unlike', { movie_id: movieId, ending_id: prev.ending_id });
    }
    await idbPut('likes', { movieId, ending_id: ending.id, target: ending.target, ts: Date.now() });
    postLike(ending.target, ending.id, +1);
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
    if (state.activeIdx >= 0 && state.activeIdx !== idx && state.activeStartedAt) {
      const prev = state.items[state.activeIdx];
      if (prev) {
        telemetryQueue.push({
          ts: Date.now(), type: 'impression',
          movie_id: prev.id,
          duration_ms: Date.now() - state.activeStartedAt,
          audio_playing: state.audioOn ? 1 : 0,
        });
      }
    }
    state.activeStartedAt = Date.now();
    state.cardsViewed += 1;
    maybeShowFeedbackHint();
    const m = state.items[idx];
    if (!m) return;
    track('card_enter', { movie_id: m.id });
    [idx + 1, idx + 2, idx + 3].forEach(j => preload(state.items[j]));
    if (m.has_audio) playAudio(m.id, m.audio_version);
    else stopAudio();
    if (idx >= state.items.length - 6) appendShuffledBatch();
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
    const m = state.items[state.activeIdx >= 0 ? state.activeIdx : 0];
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
  function maybeShowFeedbackHint() {
    if (state.cardsViewed < 3) return;
    const btn = $('#fbBtn');
    if (!btn || btn.classList.contains('shown')) return;
    if (localStorage.getItem('rwa_fb_until') &&
        Date.now() < +localStorage.getItem('rwa_fb_until')) return;
    btn.classList.add('shown');
  }
  function openFeedback() {
    track('feedback_open');
    const m = $('#fbModal');
    m.classList.add('open');
    $('#fbCustom').value = '';
    $('#fbCustomWrap').classList.remove('open');
  }
  function closeFeedback() {
    $('#fbModal').classList.remove('open');
  }
  async function submitFeedback(kind, text) {
    closeFeedback();
    track('feedback_submit', { extra: { kind, len: (text||'').length } });
    try {
      await fetch(API.feedback, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ session_id: SESSION_ID, anon_id: ANON_ID, mode: MODE, kind, text }),
        keepalive: true,
      });
    } catch {}
    localStorage.setItem('rwa_fb_until', String(Date.now() + 24*60*60*1000));
    $('#fbBtn').classList.remove('shown');
    toast('Thanks ✨');
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

  // ---------- Write your own ----------
  function openWriteOwn(item, idx) {
    track('write_own_open', { movie_id: item.id });
    state.weTarget = { contentId: item.id, title: item.title, idx };
    $('#weTitle').textContent = item.title;
    $('#weText').value = '';
    $('#weAuthor').value = localStorage.getItem('rwa_handle') || '';
    $('#weModal').classList.add('open');
  }
  function closeWriteOwn() { $('#weModal').classList.remove('open'); }
  async function submitWriteOwn() {
    const t = $('#weText').value.trim();
    const a = $('#weAuthor').value.trim();
    if (!t || !state.weTarget) return;
    if (a) localStorage.setItem('rwa_handle', a);
    closeWriteOwn();
    track('write_own_submit', { movie_id: state.weTarget.contentId, extra: { len: t.length } });
    const res = await postSubmitEnding(state.weTarget.contentId, t, a);
    if (res && res.ok) {
      toast('Submitted — AI score ' + (Math.round((res.ai_score||0)*100)/100));
      // Soft-refresh; new ending will surface in the top-3 once it earns
      // enough likes/ratings.
      setTimeout(() => { refresh().catch(()=>{}); }, 600);
    } else {
      toast('Could not submit — try again');
    }
  }
  function setupWriteOwn() {
    $('#weClose').addEventListener('click', closeWriteOwn);
    $('#weModal').addEventListener('click', (e) => {
      if (e.target.id === 'weModal') closeWriteOwn();
    });
    $('#weSubmit').addEventListener('click', submitWriteOwn);
  }

  function toast(msg) {
    const t = document.createElement('div');
    t.className = 'fb-toast';
    t.textContent = msg;
    document.body.appendChild(t);
    setTimeout(() => t.remove(), 1800);
  }

  // ---------- Boot ----------
  $('#splash').addEventListener('click', dismissSplash, { once: true });
  $('#audioBtn').addEventListener('click', () => setAudio(!state.audioOn));
  setupFeedback();
  setupWriteOwn();

  (async function boot() {
    try {
      await loadLikes();
      await fetchContent();
    } catch (e) {
      console.error('boot failed', e);
      $('#empty').classList.add('show');
      $('#empty').querySelector('p').textContent = 'Could not load content. Pull to retry.';
    }
  })();

  // Stream new endings + like counts every 60 s.
  setInterval(async () => {
    try {
      const r = await fetch(API.movies, { cache: 'no-store' });
      if (!r.ok) return;
      const data = await r.json();
      await idbPut('cache', { k: 'content', v: data, t: Date.now() });
      const byId = new Map((data.content || []).map(m => [m.id, m]));
      state.items.forEach(m => {
        const fresh = byId.get(m.id);
        if (!fresh) return;
        m.endings = fresh.endings;
        m.has_audio = fresh.has_audio;
        m.audio_version = fresh.audio_version;
        m.poster_url = m.poster_url || fresh.poster_url;
      });
      // Don't re-render the whole DOM — just refresh visible like counts on
      // existing cards so scroll position is preserved.
      state.cardEls.forEach(card => {
        const m = state.items[Number(card.dataset.idx)];
        if (!m) return;
        const tiles = $$('.ending', card).filter(x => !x.classList.contains('write-own') && !x.classList.contains('placeholder'));
        const liked = state.likeMap.get(m.id);
        tiles.forEach((el, i) => {
          const e = m.endings[i];
          if (!e) return;
          const span = $('.likes', el);
          if (span) span.textContent = formatLikes(e.likes || 0);
          el.dataset.endingId = e.id;
          el.dataset.target = e.target;
          el.classList.toggle('liked', !!(liked && liked.ending_id === e.id));
        });
      });
    } catch {}
  }, 60_000);
})();
