// Rewire service worker — cache-first for static + audio, network-first for API.
//
// v9 (1.2.0): bumped to evict stale cached bundles from earlier v1.1 build.
// Also we now ALWAYS network-first for HTML+JS so a missed bump doesn't pin
// users to an old shell.
const VERSION = 'rewire-v10';
const STATIC_CACHE = `${VERSION}-static`;
const AUDIO_CACHE = `${VERSION}-audio`;

const STATIC_ASSETS = [
  '/manifest.webmanifest',
  '/icon.svg',
];

self.addEventListener('install', (event) => {
  event.waitUntil(
    caches.open(STATIC_CACHE).then(c => c.addAll(STATIC_ASSETS)).then(() => self.skipWaiting())
  );
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches.keys().then(keys =>
      Promise.all(keys.filter(k => !k.startsWith(VERSION)).map(k => caches.delete(k)))
    ).then(() => self.clients.claim())
  );
});

// HTML + app.js: always network-first so deploys are picked up on next reload.
function isShell(url) {
  return url.pathname === '/' ||
    url.pathname === '/index.html' ||
    url.pathname === '/app.js';
}

self.addEventListener('fetch', (event) => {
  const req = event.request;
  if (req.method !== 'GET') return;

  const url = new URL(req.url);

  // Audio: cache-first, then network. Cache forever (mp3 hooks are immutable).
  if (url.pathname.startsWith('/audio/')) {
    event.respondWith((async () => {
      const cache = await caches.open(AUDIO_CACHE);
      const hit = await cache.match(req);
      if (hit) return hit;
      try {
        const resp = await fetch(req);
        if (resp.ok && resp.status === 200) cache.put(req, resp.clone());
        return resp;
      } catch {
        return new Response('', { status: 504 });
      }
    })());
    return;
  }

  // Shell (HTML + app.js): network-first. We refuse to ever pin clients to
  // a stale build again.
  if (isShell(url)) {
    event.respondWith((async () => {
      try {
        const resp = await fetch(req, { cache: 'no-store' });
        if (resp.ok) {
          const cache = await caches.open(STATIC_CACHE);
          cache.put(req, resp.clone());
        }
        return resp;
      } catch {
        const cache = await caches.open(STATIC_CACHE);
        return (await cache.match(req)) || (await cache.match('/index.html')) ||
          new Response('offline', { status: 503 });
      }
    })());
    return;
  }

  // API: network-first, fall back to last cached payload.
  if (url.pathname.startsWith('/api/')) {
    event.respondWith((async () => {
      try {
        const resp = await fetch(req);
        if (resp.ok) {
          const cache = await caches.open(STATIC_CACHE);
          cache.put(req, resp.clone());
        }
        return resp;
      } catch {
        const cache = await caches.open(STATIC_CACHE);
        const cached = await cache.match(req);
        return cached || new Response(JSON.stringify({ ok: false, offline: true }), {
          headers: { 'Content-Type': 'application/json' }, status: 503,
        });
      }
    })());
    return;
  }

  // Static: cache-first.
  event.respondWith((async () => {
    const cache = await caches.open(STATIC_CACHE);
    const hit = await cache.match(req);
    if (hit) return hit;
    try {
      const resp = await fetch(req);
      if (resp.ok) cache.put(req, resp.clone());
      return resp;
    } catch {
      return cache.match('/index.html') || new Response('offline', { status: 503 });
    }
  })());
});
