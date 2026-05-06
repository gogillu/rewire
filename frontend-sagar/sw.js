// Rewire /sagar service worker — cache-first for static + audio, network-first for API.
// Separate cache prefix from /, /abhinav so caches don't collide.
const VERSION = 'rewire-sagar-v2';
const STATIC_CACHE = `${VERSION}-static`;
const AUDIO_CACHE  = `${VERSION}-audio`;

const STATIC_ASSETS = [
  '/sagar/',
  '/sagar/index.html',
  '/sagar/app.js',
  '/sagar/manifest.webmanifest',
  '/sagar/icon.svg',
];

self.addEventListener('install', (event) => {
  event.waitUntil(
    caches.open(STATIC_CACHE).then(c => c.addAll(STATIC_ASSETS)).then(() => self.skipWaiting())
  );
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches.keys().then(keys =>
      Promise.all(keys.filter(k => !k.startsWith(VERSION) && k.startsWith('rewire-sagar-')).map(k => caches.delete(k)))
    ).then(() => self.clients.claim())
  );
});

self.addEventListener('fetch', (event) => {
  const req = event.request;
  if (req.method !== 'GET') return;

  const url = new URL(req.url);

  // Audio (shared with the direct build) — cache-first; we honor query strings
  // so the ?v=<mtime> cache-bust still works.
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

  // API: network-first, fall back to last cached payload (per /sagar route).
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

  // Static (only intercept paths under /sagar/ — let the direct build handle '/').
  if (!url.pathname.startsWith('/sagar')) return;
  event.respondWith((async () => {
    const cache = await caches.open(STATIC_CACHE);
    const hit = await cache.match(req);
    if (hit) return hit;
    try {
      const resp = await fetch(req);
      if (resp.ok) cache.put(req, resp.clone());
      return resp;
    } catch {
      return cache.match('/sagar/index.html') || new Response('offline', { status: 503 });
    }
  })());
});
