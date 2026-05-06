"""Rewire audio pipeline.

For each movie we:
  1. Ask the god LLM for the *single* most iconic, catchy song (one batch call,
     97 titles in one prompt → JSON dict of {movie_id: search_query}).
  2. yt-dlp scsearch1 → temp mp3 from SoundCloud (datacenter-friendly source).
  3. ffmpeg per-second RMS analysis → find the loudest 22-second window
     (the "most exciting beat").
  4. ffmpeg trim with 1s fade-in / 1s fade-out → 22-second mp3 hook,
     mono 96kbps to keep file size under ~270 KB.
  5. Save to data/audio/<movie_id>.mp3.

Idempotent: skips movies that already have an audio file ≥ 100 KB.
Failures are recorded in audio-progress.json with the search query that
was tried so we can iterate offline.
"""
from __future__ import annotations

import concurrent.futures
import json
import os
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from threading import Lock

REWIRE_ROOT = Path(r"C:\Users\arushi\Rewire")
DATA = REWIRE_ROOT / "data"
AUDIO_DIR = DATA / "audio"
AUDIO_DIR.mkdir(parents=True, exist_ok=True)
SESSION = Path(
    r"C:\Users\arushi\.copilot\session-state\abd0700f-1772-4351-81ba-707e1cdfc3db\files\rewire-runtime"
)
SESSION.mkdir(parents=True, exist_ok=True)

GOD_TOKEN = (Path(SESSION).parent / "god-token.txt").read_text().strip()
GOD = "https://localhost:9871"

PROGRESS_FILE = SESSION / "audio-progress.json"
LOG_FILE = SESSION / "audio.log"
QUERIES_FILE = SESSION / "audio-queries.json"

WORKERS = 4
HOOK_SEC = 22  # 22s with 2s of fades = 20s usable.
MIN_FILE_SIZE = 100_000

_progress_lock = Lock()
_progress = {
    "started_at": time.strftime("%Y-%m-%dT%H:%M:%S"),
    "total": 0,
    "completed": 0,
    "skipped_existing": 0,
    "failed": 0,
    "errors": [],
    "in_flight": 0,
    "running": True,
}


def log(msg: str) -> None:
    line = f"[{time.strftime('%H:%M:%S')}] {msg}"
    with LOG_FILE.open("a", encoding="utf-8") as fh:
        fh.write(line + "\n")
    print(line, flush=True)


def save_progress() -> None:
    with _progress_lock:
        PROGRESS_FILE.write_text(json.dumps(_progress, indent=2), encoding="utf-8")


def http_post_json(url: str, body: dict, headers: dict | None = None, timeout: float = 60.0) -> dict:
    import ssl
    ctx = ssl._create_unverified_context()
    req = urllib.request.Request(
        url,
        data=json.dumps(body).encode("utf-8"),
        headers={"Content-Type": "application/json", **(headers or {})},
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=timeout, context=ctx) as r:
        return json.loads(r.read().decode("utf-8", "replace"))


# --- Step 1: ask god LLM for the most iconic song per movie ---

QUERY_PROMPT = """For each Bollywood movie below return the single most iconic,
catchy song from that film — the one that defines the movie. Prefer the
title song or the chorus everyone hums; avoid sad ballads unless the film
is famous for one. Output ONLY a JSON object mapping movie_id -> a short
SoundCloud search query of the form "<song name> <movie title>" (no
quotes, no markdown, no explanation).

Movies:
"""


def fetch_song_queries(movies: list[dict]) -> dict[str, str]:
    if QUERIES_FILE.exists():
        try:
            cached = json.loads(QUERIES_FILE.read_text(encoding="utf-8"))
            cached = _unwrap(cached)
            if isinstance(cached, dict) and len(cached) >= len(movies) * 0.8:
                log(f"using cached song queries ({len(cached)} entries)")
                return cached
        except Exception:
            pass
    listing = "\n".join(f"  {m['id']} — {m['title']} ({m['year']})" for m in movies)
    prompt = QUERY_PROMPT + listing + "\n\nReturn the JSON object now."
    log("asking god for iconic-song queries…")
    resp = http_post_json(
        f"{GOD}/v1/invoke",
        {
            "model": "gpt-5.5",
            "prompt": prompt,
            "responseFormat": "json",
            "maxTokens": 8000,
        },
        headers={"Authorization": f"Bearer {GOD_TOKEN}"},
        timeout=180,
    )
    raw = _unwrap(resp)
    if isinstance(raw, dict):
        QUERIES_FILE.write_text(json.dumps(raw, indent=2), encoding="utf-8")
        log(f"got {len(raw)} song queries from god")
        return raw
    log("god failed to return a usable dict; falling back to title-only queries")
    return {m["id"]: f"{m['title']} title song" for m in movies}


def _unwrap(obj):
    """Repeatedly peel off god's JSON wrappers until we hit the movie-id dict."""
    seen = 0
    while seen < 6 and isinstance(obj, dict):
        # Heuristic: if every key looks like a movie-id (kebab-case, no spaces),
        # we found it.
        keys = list(obj.keys())
        if keys and all(("-" in k or k.isalnum()) and " " not in k for k in keys[:5]) \
           and not any(k in obj for k in ("response", "answer", "text", "content", "confidence")):
            return obj
        for k in ("response", "answer", "text", "content", "data"):
            if k in obj:
                obj = obj[k]
                break
        else:
            break
        seen += 1
    if isinstance(obj, str):
        s = obj.strip().strip("`")
        if s.lower().startswith("json"):
            s = s[4:].lstrip()
        try:
            return _unwrap(json.loads(s))
        except Exception:
            return None
    return obj


# --- Step 2: yt-dlp scsearch download ---


def yt_dlp_download(query: str, dest_dir: Path) -> Path | None:
    """Run yt-dlp scsearch1, return path to extracted mp3 or None."""
    template = str(dest_dir / "src.%(ext)s")
    cmd = [
        sys.executable, "-m", "yt_dlp",
        "-q", "--no-warnings", "--no-playlist",
        "--default-search", "scsearch1",
        "-x", "--audio-format", "mp3", "--audio-quality", "5",
        "-o", template, query,
    ]
    try:
        subprocess.run(cmd, capture_output=True, text=True, timeout=120, check=False)
    except subprocess.TimeoutExpired:
        return None
    matches = list(dest_dir.glob("src.mp3"))
    if matches and matches[0].stat().st_size > 30_000:
        return matches[0]
    return None


# --- Step 3: find loudest 22-second window ---


def find_hot_window(mp3: Path, window: int) -> int:
    """Return start-second of loudest window. 0 on failure."""
    cmd = [
        "ffmpeg", "-hide_banner", "-nostats", "-i", str(mp3),
        "-af",
        "asetnsamples=44100,astats=metadata=1:reset=1,"
        "ametadata=mode=print:key=lavfi.astats.Overall.RMS_level",
        "-f", "null", "-",
    ]
    try:
        r = subprocess.run(cmd, capture_output=True, text=True, timeout=120)
    except subprocess.TimeoutExpired:
        return 0
    rms: list[float] = []
    for line in r.stderr.splitlines():
        if "lavfi.astats.Overall.RMS_level" in line:
            try:
                v = float(line.split("=")[-1])
                rms.append(v if v > -120 else -120)
            except Exception:
                rms.append(-120)
    if len(rms) <= window:
        return 0
    # Find loudest sliding window. Skip the first 8s and last 8s of the song.
    skip_head = min(8, len(rms) // 6)
    skip_tail = min(8, len(rms) // 8)
    lo = skip_head
    hi = len(rms) - window - skip_tail
    if hi <= lo:
        return 0
    cur = sum(rms[lo : lo + window])
    best_sum = cur
    best_start = lo
    for i in range(lo + 1, hi):
        cur += rms[i + window - 1] - rms[i - 1]
        if cur > best_sum:
            best_sum = cur
            best_start = i
    return best_start


# --- Step 4: extract trimmed mp3 ---


def extract_clip(src: Path, dest: Path, offset: int, dur: int = HOOK_SEC) -> bool:
    cmd = [
        "ffmpeg", "-y", "-hide_banner", "-loglevel", "error",
        "-ss", str(offset), "-t", str(dur),
        "-i", str(src),
        "-af", f"afade=in:st=0:d=1,afade=out:st={dur - 1}:d=1,loudnorm=I=-16:TP=-1.5:LRA=11",
        "-ac", "2", "-ar", "44100", "-b:a", "96k",
        str(dest),
    ]
    try:
        r = subprocess.run(cmd, capture_output=True, text=True, timeout=60)
    except subprocess.TimeoutExpired:
        return False
    return dest.exists() and dest.stat().st_size > MIN_FILE_SIZE


# --- Worker ---


def process_movie(movie: dict, query: str) -> tuple[str, bool, str]:
    mid = movie["id"]
    out = AUDIO_DIR / f"{mid}.mp3"
    if out.exists() and out.stat().st_size >= MIN_FILE_SIZE:
        with _progress_lock:
            _progress["skipped_existing"] += 1
            _progress["in_flight"] -= 1
        return (mid, True, "exists")
    work = Path(tempfile.mkdtemp(prefix=f"rewire-aud-{mid}-"))
    try:
        src = yt_dlp_download(query, work)
        if not src:
            with _progress_lock:
                _progress["failed"] += 1
                _progress["errors"].append({"id": mid, "stage": "yt-dlp", "query": query})
                _progress["in_flight"] -= 1
            return (mid, False, "yt-dlp failed")
        offset = find_hot_window(src, HOOK_SEC)
        if not extract_clip(src, out, offset):
            with _progress_lock:
                _progress["failed"] += 1
                _progress["errors"].append({"id": mid, "stage": "ffmpeg", "query": query})
                _progress["in_flight"] -= 1
            return (mid, False, "ffmpeg failed")
        with _progress_lock:
            _progress["completed"] += 1
            _progress["in_flight"] -= 1
        return (mid, True, f"@{offset}s")
    finally:
        shutil.rmtree(work, ignore_errors=True)


# --- main ---


def main() -> int:
    movies = json.loads((DATA / "movies.json").read_text(encoding="utf-8"))
    queries = fetch_song_queries(movies)
    work_items: list[tuple[dict, str]] = []
    for m in movies:
        q = queries.get(m["id"]) or f"{m['title']} title song"
        work_items.append((m, q))
    _progress["total"] = len(work_items)
    _progress["in_flight"] = len(work_items)
    save_progress()
    log(f"starting audio pipeline for {len(work_items)} movies, {WORKERS} workers")
    with concurrent.futures.ThreadPoolExecutor(max_workers=WORKERS) as ex:
        futures = {ex.submit(process_movie, m, q): m["id"] for m, q in work_items}
        done = 0
        for fut in concurrent.futures.as_completed(futures):
            done += 1
            mid = futures[fut]
            try:
                _, ok, msg = fut.result()
            except Exception as e:
                ok, msg = False, f"crash: {e}"
            log(f"[{done}/{len(work_items)}] {mid}: {'ok' if ok else 'FAIL'} {msg}")
            if done % 4 == 0:
                save_progress()
    _progress["running"] = False
    _progress["finished_at"] = time.strftime("%Y-%m-%dT%H:%M:%S")
    save_progress()
    log("done")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as e:
        log(f"fatal: {e}")
        _progress["running"] = False
        _progress["fatal"] = str(e)
        save_progress()
        raise
