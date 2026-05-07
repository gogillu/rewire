// rewire-intro.js — v1.5.0
// Pure-DOM brain rewire intro. Once-per-session (sessionStorage flag).
// Tap-to-skip. Resolves a Promise after the animation completes or skip.
//
// Usage:
//   <link rel="stylesheet" href="/rewire-intro.css">
//   <script src="/rewire-intro.js"></script>
//   RewireIntro.show().then(() => { /* boot the rest */ });
(function () {
  'use strict';
  if (window.RewireIntro) return;

  const FLAG_KEY = 'rw_intro_seen_v1';
  // Brain silhouette path — single closed curve approximating two hemispheres.
  // 600x600 viewBox; centered, ~70% width.
  const BRAIN_PATH =
    'M300 110 ' +
    'C 230 110, 165 150, 150 215 ' +
    'C 95 240, 95 320, 150 345 ' +
    'C 145 405, 200 460, 260 460 ' +
    'C 270 480, 290 495, 300 495 ' +
    'C 310 495, 330 480, 340 460 ' +
    'C 400 460, 455 405, 450 345 ' +
    'C 505 320, 505 240, 450 215 ' +
    'C 435 150, 370 110, 300 110 Z ' +
    // Inner wrinkles (folds)
    'M 220 200 C 240 220, 260 230, 280 230 ' +
    'M 380 200 C 360 220, 340 230, 320 230 ' +
    'M 200 290 C 230 305, 260 305, 290 295 ' +
    'M 400 290 C 370 305, 340 305, 310 295 ' +
    'M 230 360 C 260 380, 295 385, 300 380 ' +
    'M 370 360 C 340 380, 305 385, 300 380 ' +
    'M 300 230 L 300 460';

  // Synapse points — small dots that "fire" along the brain after lines hit.
  const SYNAPSES = [
    { cx: 215, cy: 210, r: 5, cls: 's1' },
    { cx: 385, cy: 210, r: 5, cls: 's2' },
    { cx: 200, cy: 300, r: 4, cls: 's3' },
    { cx: 400, cy: 300, r: 4, cls: 's4' },
    { cx: 300, cy: 380, r: 5, cls: 's5' },
  ];

  // 8 lines projecting from offscreen into the brain at varied angles.
  // We work in viewBox 600x600 with brain centered at (300, 300).
  const LINES = [
    { x1: 60,   y1: 60,   x2: 240, y2: 240, cls: 'l1' },
    { x1: 540,  y1: 60,   x2: 360, y2: 240, cls: 'l2' },
    { x1: 60,   y1: 540,  x2: 240, y2: 360, cls: 'l3' },
    { x1: 540,  y1: 540,  x2: 360, y2: 360, cls: 'l4' },
    { x1: 0,    y1: 300,  x2: 200, y2: 300, cls: 'l5' },
    { x1: 600,  y1: 300,  x2: 400, y2: 300, cls: 'l6' },
    { x1: 300,  y1: 0,    x2: 300, y2: 200, cls: 'l7' },
    { x1: 300,  y1: 600,  x2: 300, y2: 400, cls: 'l8' },
  ];

  function buildOverlay() {
    const div = document.createElement('div');
    div.id = 'rewireIntro';
    div.setAttribute('aria-hidden', 'true');
    div.innerHTML = `
      <div class="stage">
        <svg class="lines" viewBox="0 0 600 600" preserveAspectRatio="xMidYMid meet">
          ${LINES.map(l => `<line class="${l.cls}" x1="${l.x1}" y1="${l.y1}" x2="${l.x2}" y2="${l.y2}"/>`).join('')}
        </svg>
        <svg class="brain" viewBox="0 0 600 600" preserveAspectRatio="xMidYMid meet">
          <path class="outline" d="${BRAIN_PATH}"/>
          ${SYNAPSES.map(s => `<circle class="synapse ${s.cls}" cx="${s.cx}" cy="${s.cy}" r="${s.r}"/>`).join('')}
        </svg>
        <div class="wordmark">REWIRE</div>
        <div class="tagline">alternate endings · in your head</div>
      </div>
      <div class="skip-hint">tap to skip</div>
    `;
    return div;
  }

  function alreadySeen() {
    try { return sessionStorage.getItem(FLAG_KEY) === '1'; } catch (_) { return false; }
  }
  function markSeen() {
    try { sessionStorage.setItem(FLAG_KEY, '1'); } catch (_) {}
  }

  function show(opts) {
    opts = opts || {};
    const force = !!opts.force;
    if (!force && alreadySeen()) return Promise.resolve(false);

    const reduced = matchMedia && matchMedia('(prefers-reduced-motion: reduce)').matches;
    if (reduced) { markSeen(); return Promise.resolve(false); }

    return new Promise((resolve) => {
      const el = buildOverlay();
      document.body.appendChild(el);
      let done = false;
      const finish = () => {
        if (done) return;
        done = true;
        markSeen();
        el.classList.add('fading');
        setTimeout(() => { try { el.remove(); } catch (_) {} resolve(true); }, 380);
      };
      // Auto-dismiss at 2.45s (matches CSS keyframe end + buffer).
      const auto = setTimeout(finish, 2450);
      el.addEventListener('click', () => { clearTimeout(auto); finish(); }, { once: true });
      // ESC also dismisses on desktop.
      const escHandler = (e) => { if (e.key === 'Escape') { clearTimeout(auto); finish(); } };
      window.addEventListener('keydown', escHandler, { once: true });
    });
  }

  window.RewireIntro = { show: show };
})();
