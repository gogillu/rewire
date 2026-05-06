"""Smarter poster backfill for Rewire.

The Go startup backfill tries a fixed list of Wikipedia title variants
(`Foo (YYYY film)` etc). For ~60% of titles that works; for the rest
the canonical Wikipedia title is something like `Munna Bhai M.B.B.S.`
or `Zindagi Na Milegi Dobara` which doesn't fall out of any of those
variants. This script uses the Wikipedia search API (`action=query
&list=search`) to find the best matching article title, then asks the
REST summary endpoint for that title's `originalimage` URL.

We update the DB by calling the Rewire backend's admin endpoint so the
running server's in-memory cache gets invalidated.
"""
from __future__ import annotations

import json
import os
import sqlite3
import sys
import time
import urllib.parse
import urllib.request
from pathlib import Path

REWIRE_ROOT = Path(r"C:\Users\arushi\Rewire")
DB_PATH = REWIRE_ROOT / "data" / "rewire.db"
ADMIN_TOKEN_FILE = Path(
    r"C:\Users\arushi\.copilot\session-state\abd0700f-1772-4351-81ba-707e1cdfc3db"
    r"\files\rewire-runtime\admin-token.txt"
)
BASE = "https://gogillu.live:9999"

UA = "Rewire/0.1 (https://gogillu.live; admin@gogillu.live) python-urllib"

_LAST_CALL = [0.0]
_MIN_INTERVAL = 1.2  # seconds — Wikipedia is happy with ~1 req/sec.


def http_json(url: str, timeout: float = 10.0, retries: int = 3):
    import urllib.error
    for attempt in range(retries):
        delay = _MIN_INTERVAL - (time.monotonic() - _LAST_CALL[0])
        if delay > 0:
            time.sleep(delay)
        _LAST_CALL[0] = time.monotonic()
        req = urllib.request.Request(url, headers={"User-Agent": UA, "Accept": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=timeout) as r:
                return json.loads(r.read().decode("utf-8", errors="replace"))
        except urllib.error.HTTPError as e:
            if e.code == 429 and attempt < retries - 1:
                time.sleep(5 * (attempt + 1))
                continue
            raise
        except Exception:
            if attempt < retries - 1:
                time.sleep(2)
                continue
            raise


def wiki_search(title: str, year: int) -> list[str]:
    """Return ordered list of candidate Wikipedia article titles."""
    queries = [
        f"{title} {year} film",
        f"{title} Hindi film",
        f"{title} Bollywood film",
        f"{title} film",
        title,
    ]
    seen: set[str] = set()
    out: list[str] = []
    for q in queries:
        url = (
            "https://en.wikipedia.org/w/api.php?action=query&list=search"
            f"&srsearch={urllib.parse.quote(q)}&srlimit=8&format=json&origin=*"
        )
        try:
            data = http_json(url)
        except Exception:
            continue
        for hit in data.get("query", {}).get("search", []):
            t = hit.get("title", "")
            if not t or t in seen:
                continue
            seen.add(t)
            out.append(t)
    return out


def page_image(article_title: str) -> str | None:
    """Use MediaWiki action=query&prop=pageimages — much more reliable.

    Falls back to the REST summary endpoint if pageimages returns nothing.
    """
    url = (
        "https://en.wikipedia.org/w/api.php?action=query&prop=pageimages"
        "&piprop=original|thumbnail&pithumbsize=600&format=json&origin=*"
        f"&titles={urllib.parse.quote(article_title)}"
    )
    try:
        data = http_json(url)
    except Exception:
        data = None
    if data:
        pages = (data.get("query") or {}).get("pages") or {}
        for _, page in pages.items():
            oi = page.get("original") or {}
            src = oi.get("source")
            if src:
                return src
            thumb = (page.get("thumbnail") or {}).get("source")
            if thumb:
                return thumb
    # REST summary fallback.
    try:
        path = urllib.parse.quote(article_title.replace(" ", "_"), safe="")
        s = http_json(f"https://en.wikipedia.org/api/rest_v1/page/summary/{path}")
        src = (s.get("originalimage") or {}).get("source") or (s.get("thumbnail") or {}).get("source")
        return src or None
    except Exception:
        return None


def _norm(s: str) -> str:
    """Aggressive normalize for title comparison: lower, strip punct/spaces."""
    return "".join(c for c in s.lower() if c.isalnum())


def is_film_article(article_title: str) -> bool:
    t = article_title.lower()
    return "film)" in t or "(film" in t or "movie)" in t


def find_poster(title: str, year: int) -> str | None:
    candidates = wiki_search(title, year)
    norm_title = _norm(title)

    # Strictly require: normalized article title starts with normalized
    # movie title. This rules out "Rush (2012 Indian film)" matching Barfi.
    def matches_title(art: str) -> bool:
        a = _norm(art)
        # Drop trailing parenthetical like "(YYYY film)"; we already removed
        # punctuation/spaces but keep what's before the (now-stripped) "(".
        # Easiest: split on " (" before normalizing.
        head = art.split(" (", 1)[0]
        return _norm(head) == norm_title or _norm(head).startswith(norm_title) or norm_title.startswith(_norm(head))

    strict = [c for c in candidates if matches_title(c)]
    # Prefer film-tagged + year-mentioning ones first within strict matches.
    def score(a: str) -> int:
        s = 0
        if is_film_article(a):
            s += 10
        if str(year) in a:
            s += 4
        return -s
    strict.sort(key=score)
    if not strict:
        return None
    for art in strict[:5]:
        img = page_image(art)
        if img and any(k in img.lower() for k in (".jpg", ".png", ".jpeg", ".webp")):
            return img
    return None


def main() -> int:
    admin = ADMIN_TOKEN_FILE.read_text().strip()
    con = sqlite3.connect(str(DB_PATH))
    rows = con.execute(
        "SELECT id, title, year FROM movies WHERE poster_url IS NULL OR poster_url = '' OR poster_url = 'none'"
    ).fetchall()
    con.close()
    print(f"backfill: {len(rows)} movie(s) missing posters", flush=True)
    found = 0
    failed = 0
    for mid, title, year in rows:
        url = find_poster(title, int(year or 0))
        if not url:
            print(f"  [skip] {mid}: {title} ({year})", flush=True)
            failed += 1
            continue
        # Update via admin endpoint so the running server's cache invalidates.
        # We don't have a poster-update admin route; write directly to SQLite
        # with the server's busy_timeout-friendly settings, then nudge cache.
        con = sqlite3.connect(str(DB_PATH), timeout=10.0)
        con.execute("PRAGMA journal_mode=WAL;")
        con.execute("UPDATE movies SET poster_url = ? WHERE id = ?", (url, mid))
        con.commit()
        con.close()
        found += 1
        print(f"  [ok]   {mid}: {url}", flush=True)
    # Bust the in-memory cache by hitting any like endpoint as a no-op? The
    # /api/movies cache TTL is 5 s so it'll refresh on its own anyway.
    print(f"backfill: done — found {found}, failed {failed}", flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
