// Rewire frontend — vanilla JS, no framework. Tiny, fast, IndexedDB-cached.
(function () {
  'use strict';

  const $  = (sel, root = document) => root.querySelector(sel);
  const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

  // ---------- IndexedDB (likes & history) ----------
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

  // ---------- State ----------
  const state = {
    movies: [],
    cardEls: [],
    likeMap: new Map(),         // movieId -> endingId user picked
    audioOn: false,
    yt: null,                   // YT.Player instance
    ytReady: false,
    activeIdx: -1,
    pendingSong: null,          // queued YouTube id while ytReady is false
  };

  // ---------- API ----------
  async function fetchMovies() {
    // Use cached payload first if fresh, then refresh in background.
    const cached = await idbGet('cache', 'movies');
    if (cached && Date.now() - cached.t < 1000 * 60 * 30) {
      state.movies = cached.v.movies;
      render();
      // Background refresh
      refresh().catch(() => {});
      return;
    }
    await refresh();
  }
  async function refresh() {
    const r = await fetch('/api/movies', { cache: 'no-store' });
    if (!r.ok) throw new Error('fetch movies failed');
    const data = await r.json();
    state.movies = data.movies || [];
    await idbPut('cache', { k: 'movies', v: data, t: Date.now() });
    render();
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
    const all = await new Promise(async (res, rej) => {
      const db = await openDB();
      const tx = db.transaction('likes', 'readonly');
      const r = tx.objectStore('likes').getAll();
      r.onsuccess = () => res(r.result || []);
      r.onerror = () => rej(r.error);
    });
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
    // Show first card immediately (poster fade-in).
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

    const meta = document.createElement('div');
    meta.className = 'meta';
    meta.innerHTML = `
      <div class="title">${escapeHTML(m.title)}</div>
      <div class="sub">${m.year || ''} · ★ ${(m.imdb_rating || 0).toFixed(1)} · ${escapeHTML(m.genre || '')}</div>
    `;
    card.appendChild(meta);

    const wrap = document.createElement('div');
    wrap.className = 'endings';
    const liked = state.likeMap.get(m.id);
    const slots = [0, 1, 2].map(i => m.endings && m.endings[i]);
    slots.forEach((e, i) => {
      const el = document.createElement('div');
      if (!e) {
        el.className = 'ending placeholder';
        el.innerHTML = `<div class="text">Rewriting reality…</div>
                        <div class="row"><span class="model">slot ${i + 1}</span></div>`;
      } else {
        el.className = 'ending';
        if (liked === e.id) el.classList.add('liked');
        el.dataset.endingId = e.id;
        el.innerHTML = `
          <div class="text">${escapeHTML(e.text)}</div>
          <div class="row">
            <span><i class="heart"></i><span class="likes">${e.likes || 0}</span></span>
            <span class="model">${escapeHTML(modelLabel(e.model))}</span>
          </div>
        `;
        el.addEventListener('click', () => onLike(m.id, e.id, el, card));
      }
      wrap.appendChild(el);
    });
    card.appendChild(wrap);

    if (idx === 0) {
      const hint = document.createElement('div');
      hint.className = 'swipe-hint';
      hint.textContent = 'Swipe ↑';
      card.appendChild(hint);
    }
    return card;
  }

  function modelLabel(m) {
    if (!m) return '';
    if (m.startsWith('gpt')) return 'GPT';
    if (m.startsWith('claude-opus')) return 'Claude Opus';
    if (m.startsWith('claude-sonnet')) return 'Claude Sonnet';
    if (m.startsWith('claude')) return 'Claude';
    return m;
  }

  // ---------- Like ----------
  async function onLike(movieId, endingId, el, card) {
    const prev = state.likeMap.get(movieId);
    if (prev === endingId) return;       // already liked this one — do nothing
    state.likeMap.set(movieId, endingId);
    // visual: clear sibling .liked, mark this one
    $$('.ending', card).forEach(x => x.classList.remove('liked'));
    el.classList.add('liked');
    bumpLike(el, +1);
    if (typeof prev === 'number') {
      // We don't know which sibling element matches `prev` cheaply, but bumpLike
      // on the previously selected sibling (if any) keeps counts honest.
      const siblings = $$('.ending', card);
      const prevEl = siblings.find(x => Number(x.dataset.endingId) === prev);
      if (prevEl) bumpLike(prevEl, -1);
      postLike(prev, -1).then(updateLikeCount);
    }
    await idbPut('likes', { movieId, endingId, ts: Date.now() });
    postLike(endingId, +1).then(updateLikeCount);
  }
  function bumpLike(el, delta) {
    const span = $('.likes', el);
    if (!span) return;
    const cur = parseInt(span.textContent || '0', 10) || 0;
    span.textContent = Math.max(0, cur + delta);
  }
  function updateLikeCount(resp) {
    if (!resp || typeof resp.likes !== 'number') return;
    // server's authoritative count is reflected on the next /api/movies refresh
  }

  // ---------- Observer for active card ----------
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
    state.cardEls.forEach(el => io.observe(el));
  }
  function onCardActive(idx) {
    const m = state.movies[idx];
    if (!m) return;
    // Preload neighbouring posters
    [idx - 1, idx + 1, idx + 2].forEach(j => {
      const mm = state.movies[j];
      if (mm && mm.poster_url) {
        const im = new Image();
        im.src = mm.poster_url;
      }
    });
    // Switch theme song
    if (m.youtube_id) playYouTube(m.youtube_id);
    else stopYouTube();
  }

  // ---------- YouTube IFrame API ----------
  function ensureYT() {
    if (window.YT && window.YT.Player) return;
    const tag = document.createElement('script');
    tag.src = 'https://www.youtube.com/iframe_api';
    document.head.appendChild(tag);
  }
  window.onYouTubeIframeAPIReady = function () {
    state.yt = new YT.Player('yt-host', {
      height: '1', width: '1',
      playerVars: { autoplay: 1, controls: 0, disablekb: 1, modestbranding: 1, playsinline: 1, rel: 0 },
      events: {
        onReady: () => {
          state.ytReady = true;
          state.yt.setVolume(state.audioOn ? 60 : 0);
          if (state.audioOn) state.yt.unMute(); else state.yt.mute();
          if (state.pendingSong) playYouTube(state.pendingSong);
        },
        onStateChange: (e) => {
          // loop on end
          if (e.data === YT.PlayerState.ENDED && state.yt) state.yt.playVideo();
        },
      },
    });
  };
  function playYouTube(id) {
    if (!state.ytReady) { state.pendingSong = id; ensureYT(); return; }
    state.pendingSong = null;
    try {
      state.yt.loadVideoById({ videoId: id, startSeconds: 6 });
      if (state.audioOn) state.yt.unMute(); else state.yt.mute();
    } catch {}
  }
  function stopYouTube() {
    if (state.ytReady && state.yt) try { state.yt.stopVideo(); } catch {}
  }
  function setAudio(on) {
    state.audioOn = on;
    const ic = $('#audioIcon');
    if (on) ic.innerHTML = `<path d="M3 10v4h4l5 5V5L7 10H3zm13.5 2c0-1.77-1.02-3.29-2.5-4.03v8.05c1.48-.73 2.5-2.25 2.5-4.02zM14 3.23v2.06c2.89.86 5 3.54 5 6.71s-2.11 5.85-5 6.71v2.06c4.01-.91 7-4.49 7-8.77s-2.99-7.86-7-8.77z"/>`;
    else ic.innerHTML = `<path d="M16.5 12c0-1.77-1.02-3.29-2.5-4.03v2.21l2.45 2.45c.03-.2.05-.41.05-.63zM19 12c0 .94-.2 1.82-.54 2.64l1.51 1.51C20.63 14.91 21 13.5 21 12c0-4.28-2.99-7.86-7-8.77v2.06c2.89.86 5 3.54 5 6.71zM4.27 3L3 4.27 7.73 9H3v6h4l5 5v-6.73l4.25 4.25c-.67.52-1.42.93-2.25 1.18v2.06c1.38-.31 2.63-.95 3.69-1.81L19.73 21 21 19.73l-9-9L4.27 3zM12 4L9.91 6.09 12 8.18V4z"/>`;
    if (state.ytReady && state.yt) {
      try {
        if (on) { state.yt.unMute(); state.yt.setVolume(60); state.yt.playVideo(); }
        else    { state.yt.mute(); }
      } catch {}
    }
  }

  // ---------- Splash ----------
  function dismissSplash() {
    const sp = $('#splash');
    sp.classList.add('gone');
    setTimeout(() => sp.remove(), 600);
    ensureYT();
    setAudio(true);
    // kick first card's audio if loaded
    const m = state.movies[state.activeIdx >= 0 ? state.activeIdx : 0];
    if (m && m.youtube_id) playYouTube(m.youtube_id);
  }

  // ---------- Helpers ----------
  function escapeHTML(s) {
    return String(s ?? '').replace(/[&<>"']/g, c => ({
      '&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'
    }[c]));
  }
  function generateGradient(seed) {
    const h = [...seed].reduce((a,c)=>a+c.charCodeAt(0), 0) % 360;
    return `linear-gradient(135deg, hsl(${h},60%,18%), hsl(${(h+45)%360},70%,8%) 60%, #000)`;
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
      $('#empty').querySelector('p').textContent = 'Could not load movies. Pull down to retry.';
    }
  })();

  // Refresh in the background every 60 s so new endings + like counts stream in.
  setInterval(() => { refresh().catch(() => {}); }, 60_000);
})();
