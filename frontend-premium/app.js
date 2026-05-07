// Rewire Premium frontend — vibe-filtered Instagram-style scroll +
// leaderboard. Token-gated; on first load with no token, shows the gate
// and lets the user paste it (or click Get Premium to go to /buy).
(function () {
  'use strict';
  const $  = (s, root = document) => root.querySelector(s);
  const $$ = (s, root = document) => Array.from(root.querySelectorAll(s));

  // ---------- token ----------
  function getToken() {
    return localStorage.getItem('rw_premium_token') || '';
  }
  function setToken(t) {
    if (t) localStorage.setItem('rw_premium_token', t);
    else localStorage.removeItem('rw_premium_token');
  }

  async function verifyToken(t) {
    try {
      const r = await fetch('/api/premium/verify', { headers: { 'X-Premium-Token': t } });
      return r.ok;
    } catch { return false; }
  }

  // ---------- IndexedDB ----------
  const DB_NAME = 'rewire-premium';
  function openDB() {
    return new Promise((resolve, reject) => {
      const req = indexedDB.open(DB_NAME, 1);
      req.onupgradeneeded = () => {
        const db = req.result;
        if (!db.objectStoreNames.contains('likes')) db.createObjectStore('likes', { keyPath: 'movieId' });
        if (!db.objectStoreNames.contains('cache')) db.createObjectStore('cache', { keyPath: 'k' });
      };
      req.onerror = () => reject(req.error);
      req.onsuccess = () => resolve(req.result);
    });
  }
  async function idbGet(store, key) { const db = await openDB(); return new Promise((res, rej) => { const tx = db.transaction(store, 'readonly'); const r = tx.objectStore(store).get(key); r.onsuccess = () => res(r.result); r.onerror = () => rej(r.error); }); }
  async function idbPut(store, val) { const db = await openDB(); return new Promise((res, rej) => { const tx = db.transaction(store, 'readwrite'); tx.objectStore(store).put(val); tx.oncomplete = () => res(); tx.onerror = () => rej(tx.error); }); }
  async function idbAll(store) { const db = await openDB(); return new Promise((res, rej) => { const tx = db.transaction(store, 'readonly'); const r = tx.objectStore(store).getAll(); r.onsuccess = () => res(r.result || []); r.onerror = () => rej(r.error); }); }

  // ---------- state ----------
  const state = {
    movies: [],          // [{id, title, year, ..., classic: [...], vibe: [...]}]
    cardEls: [],
    likeMap: new Map(),
    vibes: [],           // selected vibes
    cats: [],            // selected catalog categories (bollywood/hollywood/tv-in/tv-foreign)
    audioOn: false,
    audio: null,
    activeIdx: -1,
    currentSrc: null,
    activeStartedAt: 0,
    cardsViewed: 0,
    activeMovieId: null, // for community sheet binding
    activeMovie: null,
  };

  // ---------- telemetry ----------
  const SESSION_ID = (() => { let s = sessionStorage.getItem('rw_sid'); if (!s) { s = uuid(); sessionStorage.setItem('rw_sid', s); } return s; })();
  const ANON_ID = (() => { let a = localStorage.getItem('rw_aid'); if (!a) { a = uuid(); localStorage.setItem('rw_aid', a); } return a; })();
  function uuid() { if (crypto && crypto.randomUUID) return crypto.randomUUID(); return 'xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx'.replace(/[xy]/g, c => { const r = Math.random()*16|0, v = c==='x' ? r : (r&0x3|0x8); return v.toString(16); }); }
  const evQ = [];
  function track(type, fields = {}) { evQ.push({ ts: Date.now(), type, movie_id: fields.movie_id, ending_id: fields.ending_id, duration_ms: fields.duration_ms, audio_playing: state.audioOn?1:0, extra: fields.extra }); if (evQ.length >= 20) flush(); }
  async function flush(beacon) {
    if (evQ.length === 0) return;
    const batch = evQ.splice(0, evQ.length);
    const body = JSON.stringify({ session_id: SESSION_ID, anon_id: ANON_ID, os: navigator.platform || '', browser: '', device: '', screen_w: innerWidth, screen_h: innerHeight, mode: 'premium', events: batch });
    try {
      if (beacon && navigator.sendBeacon) navigator.sendBeacon('/api/events', new Blob([body], { type: 'application/json' }));
      else await fetch('/api/events', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body, keepalive: true });
    } catch {}
  }
  setInterval(() => flush(false), 5000);
  addEventListener('pagehide', () => { track('session_end'); flush(true); });

  // ---------- API ----------
  async function fetchMovies() {
    const t = getToken();
    const r = await fetch('/api/premium/movies', { headers: { 'X-Premium-Token': t } });
    if (!r.ok) throw new Error('fetch failed (' + r.status + ')');
    return await r.json();
  }
  async function postLike(endingId, vibe, delta) {
    try {
      const r = await fetch('/api/premium/like', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'X-Premium-Token': getToken() },
        body: JSON.stringify({ ending_id: endingId, vibe: !!vibe, delta }),
      });
      if (!r.ok) return null;
      return await r.json();
    } catch { return null; }
  }
  async function fetchPrefs() {
    try {
      const r = await fetch('/api/premium/prefs', { headers: { 'X-Premium-Token': getToken() } });
      if (!r.ok) return { vibes: [], categories: ['bollywood'] };
      return await r.json();
    } catch { return { vibes: [], categories: ['bollywood'] }; }
  }
  async function savePrefs(vibes, cats) {
    try {
      await fetch('/api/premium/prefs', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'X-Premium-Token': getToken() },
        body: JSON.stringify({ vibes: vibes || [], categories: (cats && cats.length) ? cats : ['bollywood'] }),
      });
    } catch {}
  }

  // ---------- ending picker ----------
  // Pool = all vibe endings filtered by selected vibes (or humour if none),
  // dedup by fingerprint (first 12 chars), pick 3 weighted toward humour.
  function pickThree(m) {
    const sel = (state.vibes && state.vibes.length) ? state.vibes : ['humour'];
    let pool = (m.vibe_endings || []).filter(e => sel.includes(e.vibe));
    if (pool.length === 0) {
      // fall back to classic 3 endings if no vibe data yet for this movie
      return (m.endings || []).slice(0, 3).map(e => ({ ...e, vibe: 'classic', isVibe: false }));
    }
    // Weighted shuffle: humour weight = 2, others = 1.
    const weighted = [];
    for (const e of pool) {
      const w = (e.vibe === 'humour') ? 2 : 1;
      for (let i = 0; i < w; i++) weighted.push(e);
    }
    const out = [];
    const seenFp = new Set();
    while (out.length < 3 && weighted.length) {
      const idx = Math.floor(Math.random() * weighted.length);
      const e = weighted[idx];
      const fp = (e.text || '').slice(0, 12).toLowerCase();
      if (!seenFp.has(fp)) {
        out.push({ ...e, isVibe: true });
        seenFp.add(fp);
      }
      weighted.splice(idx, 1);
    }
    while (out.length < 3 && (m.endings || []).length) {
      const e = m.endings[out.length];
      if (e) out.push({ ...e, vibe: 'classic', isVibe: false });
      else break;
    }
    return out;
  }

  // ---------- render ----------
  function buildCard(m, idx) {
    const card = document.createElement('section');
    card.className = 'card';
    card.dataset.idx = idx;
    card.dataset.movie = m.id;
    const poster = document.createElement('div');
    poster.className = 'poster';
    if (m.poster_url) poster.style.backgroundImage = `url("${m.poster_url}")`;
    card.appendChild(poster);
    const vig = document.createElement('div'); vig.className = 'vignette'; card.appendChild(vig);

    // Top: title + year/rating + Insta-style stats chip (parity with /direct)
    const top = document.createElement('div');
    top.className = 'top';
    const views = m.views || 0;
    const totalLikes = m.likes_total || 0;
    const conv = (m.conversion_pct != null) ? Number(m.conversion_pct).toFixed(0) : null;
    let statsHtml = '';
    if (views > 0 || totalLikes > 0) {
      statsHtml = `<div class="stats">
        <span><b>${formatLikes(views)}</b> <em>views</em></span>
        <span>♥ <b>${formatLikes(totalLikes)}</b></span>
        ${conv != null ? `<span><b>${conv}%</b> <em>liked</em></span>` : ''}
      </div>`;
    }
    top.innerHTML = `
      <div class="title">${escapeHTML(m.title)}</div>
      <div class="sub">${m.year || ''} · ★ ${(m.imdb_rating||0).toFixed(1)}</div>
      ${statsHtml}
    `;
    card.appendChild(top);

    // Endings stack
    const wrap = document.createElement('div'); wrap.className = 'endings';
    const liked = state.likeMap.get(m.id);
    const picks = pickThree(m);
    picks.forEach(e => {
      const el = document.createElement('div'); el.className = 'ending';
      const key = (e.isVibe ? 'v' : 'c') + ':' + e.id;
      if (liked === key) el.classList.add('liked');
      el.dataset.endingKey = key;
      // v1.5: no per-ending vibe label — keep the feed clean and let the
      // vibe come through via the *content* of the line.
      el.innerHTML = `
        <div class="text">${escapeHTML(e.text)}</div>
        <div class="row"><i class="heart"></i><span class="likes">${formatLikes(e.likes||0)}</span></div>
      `;
      el.addEventListener('click', () => onLike(m.id, e, el, card, idx));
      wrap.appendChild(el);
    });

    // Community pill — "+ Write your own ending"
    const community = document.createElement('div');
    community.className = 'community-pill';
    const ccount = (m.community && m.community.count) || 0;
    community.innerHTML = `<span class="plus">＋</span> Write your own ending` +
      (ccount > 0 ? ` <span class="pill-count">${formatLikes(ccount)}</span>` : '');
    community.addEventListener('click', (ev) => { ev.stopPropagation(); openCommunitySheet(m); });
    wrap.appendChild(community);

    card.appendChild(wrap);

    const brand = document.createElement('div'); brand.className = 'brand';
    brand.innerHTML = `<div class="logo">Rewire</div>`;
    card.appendChild(brand);
    return card;
  }
  function render() {
    const deck = $('#deck');
    deck.innerHTML = '';
    state.cardEls = [];
    if (!state.movies.length) { $('#empty').classList.remove('hidden'); return; }
    $('#empty').classList.add('hidden');
    const frag = document.createDocumentFragment();
    state.movies.forEach((m, i) => { const c = buildCard(m, i); frag.appendChild(c); state.cardEls.push(c); });
    deck.appendChild(frag);
    setupObserver();
    requestAnimationFrame(() => state.cardEls[0]?.classList.add('ready'));
  }
  async function onLike(movieId, e, el, card, idx) {
    const key = (e.isVibe ? 'v' : 'c') + ':' + e.id;
    const prev = state.likeMap.get(movieId);
    if (prev === key) { advance(idx + 1); return; }
    state.likeMap.set(movieId, key);
    $$('.ending', card).forEach(x => x.classList.remove('liked'));
    el.classList.add('liked');
    bumpLike(el, +1);
    track('like', { movie_id: movieId, ending_id: e.id, extra: { vibe: e.vibe } });
    if (typeof prev === 'string') {
      const sib = $$('.ending', card).find(x => x.dataset.endingKey === prev);
      if (sib) bumpLike(sib, -1);
      const [pkind, pid] = prev.split(':');
      postLike(+pid, pkind === 'v', -1);
    }
    await idbPut('likes', { movieId, endingKey: key, ts: Date.now() });
    postLike(e.id, !!e.isVibe, +1);
    setTimeout(() => advance(idx + 1), 380);
  }
  function bumpLike(el, d) {
    const span = $('.likes', el); if (!span) return;
    const cur = parseInt(span.textContent.replace(/[k,M.]/g, ''), 10) || 0;
    span.textContent = formatLikes(Math.max(0, cur + d));
  }
  function advance(i) { state.cardEls[i]?.scrollIntoView({ behavior: 'smooth', block: 'start' }); }
  function setupObserver() {
    const io = new IntersectionObserver((ents) => {
      for (const e of ents) {
        if (e.isIntersecting && e.intersectionRatio > 0.6) {
          e.target.classList.add('ready');
          const idx = +e.target.dataset.idx;
          if (idx !== state.activeIdx) { state.activeIdx = idx; onCardActive(idx); }
        }
      }
    }, { threshold: [0, 0.6, 1] });
    state._io = io;
    state.cardEls.forEach(el => io.observe(el));
  }
  function onCardActive(idx) {
    if (state.activeIdx >= 0 && state.activeStartedAt) {
      const prev = state.movies[state.activeIdx === idx ? -1 : state.activeIdx];
      if (prev) evQ.push({ ts: Date.now(), type: 'impression', movie_id: prev.id, duration_ms: Date.now() - state.activeStartedAt });
    }
    state.activeStartedAt = Date.now();
    state.cardsViewed++;
    const m = state.movies[idx];
    if (!m) return;
    track('card_enter', { movie_id: m.id });
    [idx+1, idx+2, idx+3].forEach(j => preload(state.movies[j]));
    if (m.has_audio) playAudio(m.id, m.audio_version);
    else stopAudio();
  }
  function preload(m) { if (!m) return; if (m.poster_url) { const im = new Image(); im.src = m.poster_url; } if (m.has_audio) fetch(audioUrl(m.id, m.audio_version), { cache: 'force-cache' }).catch(()=>{}); }
  function audioUrl(id, v) { return '/audio/' + id + '.mp3' + ((typeof v === 'number' && v > 0) ? '?v=' + v : ''); }
  function ensureAudio() { if (state.audio) return state.audio; const a = new Audio(); a.loop = true; a.preload = 'auto'; a.muted = !state.audioOn; a.volume = state.audioOn ? 0.65 : 0; state.audio = a; return a; }
  function playAudio(id, v) { const a = ensureAudio(); const src = audioUrl(id, v); if (state.currentSrc !== src) { state.currentSrc = src; a.src = src; } a.muted = !state.audioOn; a.volume = state.audioOn ? 0.65 : 0; if (state.audioOn) a.play().catch(()=>{}); }
  function stopAudio() { if (state.audio) try { state.audio.pause(); } catch {}; state.currentSrc = null; }
  function setAudio(on) {
    state.audioOn = on; track(on ? 'audio_on' : 'audio_off');
    const ic = $('#audioIcon');
    if (on) ic.innerHTML = `<path d="M3 10v4h4l5 5V5L7 10H3zm13.5 2c0-1.77-1.02-3.29-2.5-4.03v8.05c1.48-.73 2.5-2.25 2.5-4.02zM14 3.23v2.06c2.89.86 5 3.54 5 6.71s-2.11 5.85-5 6.71v2.06c4.01-.91 7-4.49 7-8.77s-2.99-7.86-7-8.77z"/>`;
    else ic.innerHTML = `<path d="M16.5 12c0-1.77-1.02-3.29-2.5-4.03v2.21l2.45 2.45c.03-.2.05-.41.05-.63zM19 12c0 .94-.2 1.82-.54 2.64l1.51 1.51C20.63 14.91 21 13.5 21 12c0-4.28-2.99-7.86-7-8.77v2.06c2.89.86 5 3.54 5 6.71zM4.27 3L3 4.27 7.73 9H3v6h4l5 5v-6.73l4.25 4.25c-.67.52-1.42.93-2.25 1.18v2.06c1.38-.31 2.63-.95 3.69-1.81L19.73 21 21 19.73l-9-9L4.27 3zM12 4L9.91 6.09 12 8.18V4z"/>`;
    if (state.audio) { state.audio.muted = !on; state.audio.volume = on ? 0.65 : 0; if (on) state.audio.play().catch(()=>{}); }
  }
  function escapeHTML(s) { return String(s ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c])); }
  function formatLikes(n) { if (n < 1000) return String(n); if (n < 1e6) return (n/1000).toFixed(n<1e4?1:0) + 'k'; return (n/1e6).toFixed(1) + 'M'; }
  function shuffleInPlace(a) { for (let i=a.length-1;i>0;i--) { const j=Math.floor(Math.random()*(i+1)); [a[i],a[j]]=[a[j],a[i]]; } }

  async function loadLikes() { try { const all = await idbAll('likes'); for (const r of all) state.likeMap.set(r.movieId, r.endingKey); } catch {} }

  // ---------- Vibe + Categories panel ----------
  function openVibePanel() { paintVibePills(); paintCatPills(); $('#vibePanel').classList.add('open'); track('vibe_panel_open'); }
  function paintVibePills() {
    $$('.vibe-pill[data-vibe]').forEach(b => b.classList.toggle('on', state.vibes.includes(b.dataset.vibe)));
  }
  function paintCatPills() {
    $$('.vibe-pill[data-cat]').forEach(b => b.classList.toggle('on', state.cats.includes(b.dataset.cat)));
  }
  function setupVibePanel() {
    $('#vibeBtn').addEventListener('click', openVibePanel);
    // Vibe pills (max 3 sticky)
    $$('.vibe-pill[data-vibe]').forEach(b => b.addEventListener('click', () => {
      const v = b.dataset.vibe;
      const i = state.vibes.indexOf(v);
      if (i >= 0) state.vibes.splice(i, 1);
      else if (state.vibes.length < 3) state.vibes.push(v);
      else { state.vibes.shift(); state.vibes.push(v); }
      paintVibePills();
    }));
    // Category pills (multi-select, no cap)
    $$('.vibe-pill[data-cat]').forEach(b => b.addEventListener('click', () => {
      const v = b.dataset.cat;
      const i = state.cats.indexOf(v);
      if (i >= 0) state.cats.splice(i, 1);
      else state.cats.push(v);
      paintCatPills();
    }));
    $('#vibeSave').addEventListener('click', async () => {
      track('vibe_save', { extra: { vibes: state.vibes.join(','), cats: state.cats.join(',') } });
      await savePrefs(state.vibes, state.cats);
      $('#vibePanel').classList.remove('open');
      // Re-fetch with new prefs.
      try { const j = await fetchMovies(); applyMovies(j); } catch {}
    });
  }

  // ---------- Community endings sheet ----------
  // The /api/community/* endpoints are NOT premium-gated on the backend, so
  // we can reuse them as-is.
  async function openCommunitySheet(m) {
    state.activeMovieId = m.id;
    state.activeMovie = m;
    track('community_open', { movie_id: m.id });
    $('#communityTitle').textContent = 'Community endings · ' + m.title;
    $('#communityList').innerHTML = '<div style="opacity:.5;font-size:13px;padding:8px 0">Loading…</div>';
    $('#communitySheet').classList.add('open');
    $('#communityText').value = '';
    try {
      const r = await fetch('/api/community/endings?movie_id=' + encodeURIComponent(m.id));
      if (!r.ok) { $('#communityList').innerHTML = '<div style="opacity:.5;font-size:13px;padding:8px 0">No entries yet — be the first.</div>'; return; }
      const j = await r.json();
      const items = j.items || [];
      if (items.length === 0) {
        $('#communityList').innerHTML = '<div style="opacity:.5;font-size:13px;padding:8px 0">No entries yet — be the first.</div>';
      } else {
        $('#communityList').innerHTML = items.map(buildCommunityItem).join('');
        bindCommunityItemHandlers();
      }
    } catch (e) {
      $('#communityList').innerHTML = '<div style="opacity:.5;font-size:13px;padding:8px 0">Couldn\'t load: ' + escapeHTML(e.message) + '</div>';
    }
  }
  function buildCommunityItem(it) {
    const liked = communityIsLiked(it.id);
    const myRating = communityMyRating(it.id);
    const stars = [1,2,3,4,5].map(n => `<span class="${n <= myRating ? 'filled' : ''}" data-rating="${n}">★</span>`).join('');
    return `<div class="ce-item" data-id="${it.id}">
      <div class="ce-text">${escapeHTML(it.text)}</div>
      <div class="ce-meta">
        <span class="author">${escapeHTML(it.author || 'anon')}</span>
        <span class="heart-mini ${liked ? 'liked' : ''}" data-action="like">♥ <span class="ce-likes">${formatLikes(it.likes || 0)}</span></span>
        <div class="stars" data-action="rate">${stars}</div>
      </div>
    </div>`;
  }
  function bindCommunityItemHandlers() {
    $$('#communityList .ce-item').forEach(item => {
      const id = +item.dataset.id;
      const heart = item.querySelector('.heart-mini');
      if (heart) heart.addEventListener('click', () => communityLike(id, item));
      item.querySelectorAll('.stars span').forEach(s => {
        s.addEventListener('click', () => communityRate(id, +s.dataset.rating, item));
      });
    });
  }
  function communityKey(id) { return 'rw_ce_like_' + id; }
  function communityRateKey(id) { return 'rw_ce_rate_' + id; }
  function communityIsLiked(id) { try { return localStorage.getItem(communityKey(id)) === '1'; } catch { return false; } }
  function communityMyRating(id) { try { return parseInt(localStorage.getItem(communityRateKey(id)) || '0', 10); } catch { return 0; } }
  async function communityLike(id, itemEl) {
    const wasLiked = communityIsLiked(id);
    const delta = wasLiked ? -1 : 1;
    try {
      const r = await fetch('/api/community/like-ending', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ ending_id: id, delta }),
      });
      if (!r.ok) return;
      const j = await r.json();
      try {
        if (delta > 0) localStorage.setItem(communityKey(id), '1');
        else localStorage.removeItem(communityKey(id));
      } catch {}
      const heart = itemEl.querySelector('.heart-mini');
      heart.classList.toggle('liked', delta > 0);
      const span = itemEl.querySelector('.ce-likes');
      if (span && j.likes != null) span.textContent = formatLikes(j.likes);
      track('community_like', { movie_id: state.activeMovieId, ending_id: id, extra: { delta } });
    } catch {}
  }
  async function communityRate(id, rating, itemEl) {
    try {
      const r = await fetch('/api/community/rate-ending', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ ending_id: id, rating }),
      });
      if (!r.ok) return;
      try { localStorage.setItem(communityRateKey(id), String(rating)); } catch {}
      itemEl.querySelectorAll('.stars span').forEach(s => {
        s.classList.toggle('filled', +s.dataset.rating <= rating);
      });
      track('community_rate', { movie_id: state.activeMovieId, ending_id: id, extra: { rating } });
    } catch {}
  }
  async function communitySubmit() {
    const txt = ($('#communityText').value || '').trim();
    if (!txt || !state.activeMovieId) return;
    const author = ($('#communityAuthor').value || '').trim().slice(0, 32);
    try {
      $('#communitySubmit').disabled = true;
      const r = await fetch('/api/community/submit-ending', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ movie_id: state.activeMovieId, text: txt.slice(0, 200), author }),
      });
      if (!r.ok) { alert('Could not submit (' + r.status + ').'); return; }
      track('community_submit', { movie_id: state.activeMovieId });
      $('#communityText').value = '';
      // Refresh list with new entry on top.
      openCommunitySheet(state.activeMovie);
    } catch (e) {
      alert('Submit failed: ' + e.message);
    } finally {
      $('#communitySubmit').disabled = false;
    }
  }
  function setupCommunity() {
    $('#communityClose').addEventListener('click', () => $('#communitySheet').classList.remove('open'));
    $('#communitySubmit').addEventListener('click', communitySubmit);
    $('#communitySheet').addEventListener('click', (e) => {
      if (e.target.id === 'communitySheet') $('#communitySheet').classList.remove('open');
    });
  }

  // ---------- Leaderboard panel ----------
  async function openLBPanel() {
    track('lb_open');
    $('#lbPanel').classList.add('open');
    $('#lbRows').innerHTML = 'Loading…';
    try {
      const r = await fetch('/api/premium/leaderboard', { headers: { 'X-Premium-Token': getToken() } });
      if (!r.ok) { $('#lbRows').innerHTML = 'Could not load (' + r.status + ').'; return; }
      const j = await r.json();
      const rows = (j.rows || []).slice(0, 100);
      $('#lbRows').innerHTML = rows.map((r, i) => `
        <div class="lb-row">
          <div class="pos">${i + 1}</div>
          <div class="poster" style="background-image:url('${r.poster_url || ''}')"></div>
          <div class="info">
            <div class="t">${escapeHTML(r.title)} <span style="opacity:.55;font-weight:400">${r.year || ''}</span></div>
            <div class="e">${escapeHTML(r.text)}</div>
          </div>
          <div class="likes">${formatLikes(r.likes || 0)}</div>
        </div>
      `).join('');
    } catch (e) {
      $('#lbRows').innerHTML = 'Error: ' + e.message;
    }
  }
  function setupLB() { $('#lbBtn').addEventListener('click', openLBPanel); }

  // ---------- Closes ----------
  $$('[data-close]').forEach(b => b.addEventListener('click', () => {
    const k = b.dataset.close;
    if (k === 'vibe') $('#vibePanel').classList.remove('open');
    if (k === 'lb') $('#lbPanel').classList.remove('open');
  }));

  // ---------- Boot ----------
  async function applyMovies(payload) {
    const arr = (payload.movies || []).slice();
    shuffleInPlace(arr);
    state.movies = arr;
    state.vibes = (payload.vibes || []);
    render();
    for (let i = 0; i < 5; i++) preload(arr[i]);
  }

  async function boot() {
    // v1.5 — brain rewire intro before any token gate logic.
    if (window.RewireIntro) { try { await window.RewireIntro.show(); } catch {} }

    // Token from URL ?t= overrides; useful for the email "click to unlock" UX.
    const urlT = new URLSearchParams(location.search).get('t');
    if (urlT) { setToken(urlT); history.replaceState({}, '', '/premium'); }
    let t = getToken();
    let okay = !!t && await verifyToken(t);
    if (!okay) {
      $('#tokenGate').classList.add('open');
      $('#tokenUnlock').addEventListener('click', async () => {
        const v = $('#tokenInput').value.trim();
        if (!v) return;
        const ok = await verifyToken(v);
        if (!ok) { alert('Token rejected. Check the email or contact admin@gogillu.live.'); return; }
        setToken(v);
        $('#tokenGate').classList.remove('open');
        track('token_unlocked');
        boot();
      });
      return;
    }
    track('premium_open');
    setupVibePanel();
    setupLB();
    setupCommunity();
    $('#audioBtn').addEventListener('click', () => setAudio(!state.audioOn));
    await loadLikes();
    const prefs = await fetchPrefs();
    state.vibes = prefs.vibes || [];
    state.cats  = (prefs.categories && prefs.categories.length) ? prefs.categories : ['bollywood'];
    try {
      const j = await fetchMovies();
      applyMovies(j);
      // Auto-tap to enable audio is gated by user gesture; we just leave it muted.
    } catch (e) {
      $('#empty').textContent = 'Could not load: ' + e.message;
    }
  }
  boot();
})();
