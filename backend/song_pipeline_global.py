"""Rewire audio pipeline (global catalog).

Sister of song_pipeline.py, but for Hollywood movies + TV series (Indian
and Foreign) — i.e. the rows added by extend_catalog. Reads movie ids and
titles directly from the SQLite db (so it picks up future titles
automatically), uses a curated SEARCH_OVERRIDES table for the most iconic
themes (so we get the recognizable hook, not a random soundtrack cue),
and falls back to "<title> theme song" for anything not in the curated
table.

Idempotent: skips movies that already have an audio file ≥ 100 KB.
"""
from __future__ import annotations

import concurrent.futures
import json
import os
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import time
from pathlib import Path
from threading import Lock

REWIRE_ROOT = Path(r"C:\Users\arushi\Rewire")
DATA = REWIRE_ROOT / "data"
DB_PATH = DATA / "rewire.db"
AUDIO_DIR = DATA / "audio"
AUDIO_DIR.mkdir(parents=True, exist_ok=True)
SESSION = Path(
    r"C:\Users\arushi\.copilot\session-state\abd0700f-1772-4351-81ba-707e1cdfc3db\files\rewire-runtime"
)
SESSION.mkdir(parents=True, exist_ok=True)

PROGRESS_FILE = SESSION / "audio-global-progress.json"
LOG_FILE = SESSION / "audio-global.log"

WORKERS = 3
HOOK_SEC = 22
MIN_FILE_SIZE = 100_000

# Curated search queries — title-of-track + show/movie name. These are
# the most recognizable hooks. yt-dlp scsearch1 picks the top result.
SEARCH_OVERRIDES: dict[str, str] = {
    # --- Hollywood movies ---
    "the-shawshank-redemption":  "Stoic Theme Shawshank Redemption Thomas Newman",
    "the-godfather":             "Speak Softly Love Godfather Nino Rota",
    "the-godfather-part-ii":     "Godfather Part II main theme Nino Rota",
    "the-dark-knight":           "Why So Serious Dark Knight Hans Zimmer",
    "12-angry-men":              "12 Angry Men theme suite",
    "schindlers-list":           "Schindlers List Theme John Williams",
    "lord-of-the-rings-return-of-the-king":
        "Into the West Annie Lennox Return of the King",
    "lord-of-the-rings-fellowship":
        "Concerning Hobbits Lord of the Rings Howard Shore",
    "pulp-fiction":              "Misirlou Dick Dale Pulp Fiction",
    "fight-club":                "Where Is My Mind Pixies Fight Club",
    "forrest-gump":              "Forrest Gump Suite Alan Silvestri",
    "inception":                 "Time Inception Hans Zimmer",
    "interstellar":              "Cornfield Chase Interstellar Hans Zimmer",
    "the-matrix":                "Clubbed To Death Matrix Rob Dougan",
    "the-lion-king":             "Circle of Life Lion King",
    "gladiator":                 "Now We Are Free Gladiator Hans Zimmer",
    "back-to-the-future":        "Back to the Future Theme Alan Silvestri",
    "the-departed":              "I am Shipping Up to Boston Dropkick Murphys",
    "good-will-hunting":         "Good Will Hunting theme Danny Elfman",
    "the-prestige":              "Are You Watching Closely Prestige David Julyan",
    "se7en":                     "Se7en Closer Nine Inch Nails",
    "no-country-for-old-men":    "No Country For Old Men theme Carter Burwell",
    "memento":                   "Memento Theme David Julyan",
    "joker-2019":                "Joker Stairs Hildur Gudnadottir",
    "whiplash":                  "Caravan Whiplash drum solo",
    "parasite":                  "Parasite Belt of Faith Jung Jae-il",
    "the-truman-show":           "Truman Sleeps Truman Show Burkhard Dallwitz",
    "the-silence-of-the-lambs":  "Silence of the Lambs theme Howard Shore",

    # --- Hollywood TV series ---
    "breaking-bad":              "Breaking Bad Theme Dave Porter",
    "game-of-thrones":           "Main Title Game of Thrones Ramin Djawadi",
    "the-wire":                  "The Wire opening theme Tom Waits",
    "the-sopranos":              "Woke Up This Morning Sopranos",
    "succession":                "Succession Main Title Theme Nicholas Britell",
    "succession-tv2":            "Succession Main Title Theme Nicholas Britell",
    "chernobyl":                 "Chernobyl theme Hildur Gudnadottir",
    "the-office-us":             "The Office theme song Jay Ferguson",
    "friends":                   "Ill Be There For You Friends Rembrandts",
    "better-call-saul":          "Better Call Saul main title",
    "stranger-things":           "Stranger Things Main Theme Kyle Dixon",
    "the-witcher":               "Toss A Coin To Your Witcher",
    "money-heist":               "Bella Ciao Money Heist La Casa de Papel",
    "squid-game":                "Way Back Then Squid Game",
    "dark":                      "Goodbye Apollo Dark Netflix Ben Frost",
    "fleabag":                   "Fleabag theme Isobel Waller-Bridge",
    "true-detective":            "Far From Any Road True Detective Handsome Family",
    "westworld":                 "Westworld main title Ramin Djawadi",
    "mr-robot":                  "Mr Robot Main Title Mac Quayle",
    "the-mandalorian":           "The Mandalorian theme Ludwig Goransson",
    "house-of-the-dragon":       "House of the Dragon main title Ramin Djawadi",
    "narcos":                    "Tuyo Narcos opening Rodrigo Amarante",
    "peaky-blinders":            "Red Right Hand Nick Cave Peaky Blinders",
    "sherlock":                  "Sherlock theme David Arnold",
    "the-crown":                 "The Crown Main Title Hans Zimmer",
    "ted-lasso":                 "Ted Lasso Theme Marcus Mumford",
    "house-md":                  "Teardrop Massive Attack House MD",
    "lost":                      "Lost main theme Michael Giacchino",
    "when-they-see-us":          "When They See Us soundtrack Kris Bowers",
    "the-boys":                  "The Boys main title Christopher Lennertz",
    "watchmen":                  "Watchmen Trent Reznor Atticus Ross",
    "true-blood":                "Bad Things Jace Everett True Blood",

    # --- Indian TV series ---
    "scam-1992":                 "Scam 1992 theme Achint Thakkar",
    "panchayat":                 "Panchayat title song Anurag Saikia",
    "kota-factory":              "Kota Factory theme song",
    "tvf-pitchers":              "TVF Pitchers Tu Bhi Sahi Hai",
    "tvf-tripling":              "Tripling theme song Sahi Hai",
    "aspirants":                 "Aspirants TVF dheere dheere",
    "flames":                    "Flames TVF theme song",
    "mirzapur":                  "Mirzapur theme Mirzapur background music",
    "asur":                      "Asur theme song",
    "sacred-games":              "Sacred Games theme",
    "delhi-crime":               "Delhi Crime theme",
    "family-man":                "Family Man Bharat Theme",
    "the-family-man":            "Family Man Bharat Theme",

    # --- World cinema / TV that may be ambiguous ---
    "spirited-away":             "One Summers Day Spirited Away Joe Hisaishi",
    "city-of-god":               "City of God main theme Antonio Pinto",
    "amelie":                    "Comptine d'un autre été Amelie Yann Tiersen",
    "oldboy":                    "Oldboy Last Waltz Jo Yeong-wook",
}


_lock = Lock()
_progress: dict = {
    "started_at": time.strftime("%Y-%m-%dT%H:%M:%S"),
    "total": 0,
    "completed": 0,
    "skipped_existing": 0,
    "failed": 0,
    "errors": [],
    "running": True,
}


def log(msg: str) -> None:
    line = f"[{time.strftime('%H:%M:%S')}] {msg}"
    with LOG_FILE.open("a", encoding="utf-8") as fh:
        fh.write(line + "\n")
    print(line, flush=True)


def save_progress() -> None:
    with _lock:
        PROGRESS_FILE.write_text(json.dumps(_progress, indent=2), encoding="utf-8")


def list_global_movies() -> list[tuple[str, str]]:
    con = sqlite3.connect(DB_PATH)
    cur = con.cursor()
    cur.execute(
        """
        SELECT id, title, region, kind FROM movies
        WHERE region != 'bollywood' OR kind != 'movie'
        ORDER BY region, kind, imdb_rating DESC
        """
    )
    rows = [(r[0], r[1]) for r in cur.fetchall()]
    con.close()
    return rows


def yt_dlp_download(query: str, dest_dir: Path) -> Path | None:
    template = str(dest_dir / "src.%(ext)s")
    cmd = [
        sys.executable, "-m", "yt_dlp",
        "-q", "--no-warnings", "--no-playlist",
        "--default-search", "scsearch1",
        "-x", "--audio-format", "mp3", "--audio-quality", "5",
        "-o", template, query,
    ]
    try:
        subprocess.run(cmd, capture_output=True, text=True, timeout=180, check=False)
    except subprocess.TimeoutExpired:
        return None
    for p in dest_dir.iterdir():
        if p.suffix.lower() == ".mp3":
            return p
    return None


def find_hot_window(src: Path, length_sec: int) -> int:
    cmd = [
        "ffmpeg", "-hide_banner", "-nostats", "-i", str(src),
        "-af", "astats=metadata=1:reset=1,ametadata=print:key=lavfi.astats.Overall.RMS_level",
        "-f", "null", "-",
    ]
    try:
        out = subprocess.run(cmd, capture_output=True, text=True, timeout=60).stderr
    except subprocess.TimeoutExpired:
        return 30
    rms_per_sec: list[float] = []
    cur_t = 0.0
    cur_rms: list[float] = []
    for line in out.splitlines():
        line = line.strip()
        if line.startswith("frame:"):
            try:
                t = float(line.split("pts_time:")[1].split()[0])
            except Exception:
                continue
            sec = int(t)
            while len(rms_per_sec) < sec:
                rms_per_sec.append(min(cur_rms) if cur_rms else -90.0)
                cur_rms = []
            cur_t = t
        elif "Overall.RMS_level=" in line:
            try:
                v = float(line.split("=")[1])
                cur_rms.append(v)
            except Exception:
                pass
    if not rms_per_sec or len(rms_per_sec) < length_sec + 5:
        return 30
    best, best_sum = 5, -1e18
    for s in range(5, len(rms_per_sec) - length_sec):
        ss = sum(rms_per_sec[s:s + length_sec])
        if ss > best_sum:
            best_sum = ss
            best = s
    return best


def extract_clip(src: Path, dst: Path, offset_sec: int) -> bool:
    fade_in = 1
    fade_out = 1
    duration = HOOK_SEC
    afilter = f"afade=t=in:st=0:d={fade_in},afade=t=out:st={duration - fade_out}:d={fade_out}"
    cmd = [
        "ffmpeg", "-hide_banner", "-nostats", "-y",
        "-ss", str(offset_sec), "-i", str(src), "-t", str(duration),
        "-af", afilter, "-ac", "1", "-b:a", "96k",
        str(dst),
    ]
    try:
        r = subprocess.run(cmd, capture_output=True, text=True, timeout=60)
    except subprocess.TimeoutExpired:
        return False
    return dst.exists() and dst.stat().st_size > MIN_FILE_SIZE


def process(mid: str, title: str) -> tuple[str, bool, str]:
    out = AUDIO_DIR / f"{mid}.mp3"
    if out.exists() and out.stat().st_size > MIN_FILE_SIZE:
        with _lock:
            _progress["skipped_existing"] += 1
        return (mid, True, "exists")
    query = SEARCH_OVERRIDES.get(mid) or f"{title} theme song"
    work = Path(tempfile.mkdtemp(prefix=f"rwa-{mid}-"))
    try:
        src = yt_dlp_download(query, work)
        if not src:
            with _lock:
                _progress["failed"] += 1
                _progress["errors"].append({"id": mid, "stage": "yt-dlp", "query": query})
            return (mid, False, "yt-dlp failed")
        offset = find_hot_window(src, HOOK_SEC)
        if not extract_clip(src, out, offset):
            with _lock:
                _progress["failed"] += 1
                _progress["errors"].append({"id": mid, "stage": "ffmpeg", "query": query})
            return (mid, False, "ffmpeg failed")
        with _lock:
            _progress["completed"] += 1
        return (mid, True, f"@{offset}s")
    finally:
        shutil.rmtree(work, ignore_errors=True)


def main() -> int:
    items = list_global_movies()
    _progress["total"] = len(items)
    save_progress()
    log(f"global audio pipeline: {len(items)} candidates, {WORKERS} workers")
    with concurrent.futures.ThreadPoolExecutor(max_workers=WORKERS) as ex:
        futures = {ex.submit(process, mid, title): mid for mid, title in items}
        done = 0
        for fut in concurrent.futures.as_completed(futures):
            done += 1
            mid = futures[fut]
            try:
                _, ok, msg = fut.result()
            except Exception as e:
                ok, msg = False, f"crash: {e}"
            log(f"[{done}/{len(items)}] {mid}: {'ok' if ok else 'FAIL'} {msg}")
            if done % 5 == 0:
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
