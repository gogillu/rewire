// Rewire (Abhinav build) service worker.
// Scoped to /abhinav/ so it doesn't fight the main app's SW.
const VERSION = 'rewire-abhinav-v1';
const STATIC_CACHE = `${VERSION}-static`;
const AUDIO_CACHE = `${VERSION}-audio`;

const STATIC_ASSETS = [
  '/abhinav/',
  '/abhinav/index.html',
  '/abhinav/app.js',
  '/abhinav/manifest.webmanifest',
  '/abhinav/icon.svg',
];

self.addEventListener('install', (event) => {
  event.waitUntil(
    caches.open(STATIC_CACHE).then(c => c.addAll(STATIC_ASSETS)).then(() => self.skipWaiting())
  );
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches.keys().then(keys =>
      Promise.all(keys.filter(k => k.startsWith('rewire-abhinav-') && !k.startsWith(VERSION)).map(k => caches.delete(k)))
    ).then(() => self.clients.claim())
  );
});

self.addEventListener('fetch', (event) => {
  const req = event.request;
  if (req.method !== 'GET') return;

  const url = new URL(req.url);

  // Audio is shared with main app — cache-first.
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

  // Abhinav APIs: network-first, fall back to cache.
  if (url.pathname.startsWith('/api/abhinav/') || url.pathname.startsWith('/api/events') || url.pathname.startsWith('/api/feedback')) {
    event.respondWith((async () => {
      try {
        const resp = await fetch(req);
        if (resp.ok && req.method === 'GET') {
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

  // Static (limited to /abhinav/* per the SW scope).
  if (url.pathname.startsWith('/abhinav')) {
    event.respondWith((async () => {
      const cache = await caches.open(STATIC_CACHE);
      const hit = await cache.match(req);
      if (hit) return hit;
      try {
        const resp = await fetch(req);
        if (resp.ok) cache.put(req, resp.clone());
        return resp;
      } catch {
        return cache.match('/abhinav/index.html') || new Response('offline', { status: 503 });
      }
    })());
  }
});
