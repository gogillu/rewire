// Rewire frontend — vanilla JS. Random shuffle on every page load,
// local /audio/<id>.mp3 playback (loop), persistent like history.
(function () {
  'use strict';

  const $  = (sel, root = document) => root.querySelector(sel);
  const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

  // ---------- IndexedDB ----------
  const DB_NAME = 'rewire';
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
    audioOn: false,
    audio: null,           // single HTMLAudio element shared across cards
    activeIdx: -1,
    currentSrc: null,
  };

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

    // Endings on TOP — three stacked cards. Like history persists.
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

    // BOTTOM — movie title + Rewire logo (no longer overlapping content)
    const bot = document.createElement('div');
    bot.className = 'bottom';
    bot.innerHTML = `
      <div class="title">${escapeHTML(m.title)}</div>
      <div class="sub">${m.year || ''} · ★ ${(m.imdb_rating || 0).toFixed(1)}</div>
      <div class="logo">Rewire</div>
    `;
    card.appendChild(bot);
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
    if (typeof prev === 'number') {
      const siblings = $$('.ending', card);
      const prevEl = siblings.find(x => Number(x.dataset.endingId) === prev);
      if (prevEl) bumpLike(prevEl, -1);
      postLike(prev, -1);
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
    const m = state.movies[idx];
    if (!m) return;
    [idx + 1, idx + 2, idx + 3].forEach(j => preload(state.movies[j]));
    if (m.has_audio) playAudio(m.id);
    else stopAudio();
    // Infinite scroll: when within 6 cards of the end, shuffle + append.
    if (idx >= state.movies.length - 6) appendShuffledBatch();
  }
  function preload(m) {
    if (!m) return;
    if (m.poster_url) { const im = new Image(); im.src = m.poster_url; }
    if (m.has_audio) {
      // Hint to the SW + browser cache.
      fetch(`/audio/${m.id}.mp3`, { cache: 'force-cache' }).catch(() => {});
    }
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
  function playAudio(id) {
    const a = ensureAudio();
    const src = `/audio/${id}.mp3`;
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
    if (m && m.has_audio) playAudio(m.id);
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

  // ---------- Boot ----------
  $('#splash').addEventListener('click', dismissSplash, { once: true });
  $('#audioBtn').addEventListener('click', () => setAudio(!state.audioOn));

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
      });
    } catch {}
  }, 60_000);
})();
