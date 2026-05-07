"""
Rewire Premium — vibe ending pre-generator.

For each (movie × vibe) tuple we ask the god backend to write 3 short
alternate endings (≤ 12 words) tagged with that vibe. Result is POSTed
to the rewire backend's admin upsert endpoint.

Resumable / idempotent:
  - Tracks completion in `vibe-progress.json`. Restart-safe.
  - Backend `vibe_endings` has UNIQUE(movie_id, vibe, variant) so re-runs
    overwrite duplicates safely.
  - Hardened: timeouts, retries, JSON parse fallback, model rotation.

Resource budget:
  - 100 movies × 6 vibes = 600 god calls. ~25-40 s each → ~5 hours
    sequential, ~70 min with 5 workers.
"""
from __future__ import annotations

import argparse
import json
import os
import random
import re
import sys
import threading
import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from pathlib import Path

import urllib3
import requests

urllib3.disable_warnings(urllib3.exceptions.InsecureRequestWarning)

ROOT = Path(__file__).resolve().parent.parent
SESSION = Path(r"C:\Users\arushi\.copilot\session-state\abd0700f-1772-4351-81ba-707e1cdfc3db\files")
RUNTIME = SESSION / "rewire-runtime"
RUNTIME.mkdir(parents=True, exist_ok=True)

GOD_TOKEN = (SESSION / "god-token.txt").read_text(encoding="utf-8").strip()
ADMIN_TOK = (RUNTIME / "admin-token.txt").read_text(encoding="utf-8").strip()
GOD_URL = "https://localhost:9871/v1/invoke?actionablesContext=false"
REWIRE_BASE = "https://localhost:9999"
PROGRESS = RUNTIME / "vibe-progress.json"
LOG = RUNTIME / "vibe.log"

VIBES = ["humour", "emotional", "controversial", "sad", "happy", "impossible"]
MODELS = ["gpt-5.4-mini", "claude-haiku-4.5", "claude-sonnet-4.6"]

# Fewer-than-12-word alternate endings, tagged by vibe.
# v1.5: prompt is region-agnostic (Bollywood + Hollywood + TV) and pushes
# for VISCERAL, EXTREME endings. The user wanted endings that hit hard,
# never feel templated.
PROMPT_TMPL = """\
You are writing short, VISCERAL alternate endings for the title
"{title}" ({year}, {genre}).

Synopsis: {synopsis}
Actual ending: {actual}

Write **exactly 3** alternate endings with the vibe: **{vibe_label}**.
Each ending MUST be:
  - ≤ 12 words. Punchy. No fat.
  - A complete declarative sentence ending with a period.
  - Distinctly different from the others — different *event*, not just rephrased.
  - Specifically about THIS title's characters/setting (use real names).
  - Vibe = {vibe_label}: {vibe_hint}.
  - INTENSE. Make the reader's brain twitch. No safe, hedged, generic lines.

Do NOT explain. Do NOT use bullets. Do NOT number them in the output.
Do NOT add "Variant 1:" or labels. Just the JSON.
Output strictly as JSON:

{{"endings": ["sentence one.", "sentence two.", "sentence three."]}}
"""

VIBE_HINTS = {
    "humour":        "MAXIMUM absurd comedy — slapstick, irony, deadpan twist. Make readers laugh out loud.",
    "emotional":     "raw, gut-punch intimacy — a tear-jerker line that sneaks past defenses.",
    "controversial": "edgy, provocative, taboo, polarising — bold without being cruel.",
    "sad":           "devastating, heartbreaking, quiet tragedy — leave the reader hollow.",
    "happy":         "wholesome, joyous, miraculous — sunshine of an ending that almost feels unfair.",
    "impossible":    "reality-bending, sci-fi, supernatural, time-loops, multiversal madness.",
}

VIBE_LABEL = {
    "humour":        "EXTREME HUMOUR",
    "emotional":     "DEEPLY EMOTIONAL",
    "controversial": "BOLDLY CONTROVERSIAL",
    "sad":           "PROFOUNDLY SAD",
    "happy":         "EXTREMELY HAPPY",
    "impossible":    "REALITY-BENDING IMPOSSIBLE",
}


def log(msg: str) -> None:
    line = f"[{time.strftime('%H:%M:%S')}] {msg}"
    try:
        print(line, flush=True)
    except UnicodeEncodeError:
        # Windows cp1252 stdout — strip non-ASCII.
        try:
            print(line.encode("ascii", "replace").decode("ascii"), flush=True)
        except Exception:
            pass
    try:
        with LOG.open("a", encoding="utf-8") as f:
            f.write(line + "\n")
    except OSError:
        pass


prog_lock = threading.Lock()


def load_progress() -> dict:
    if PROGRESS.exists():
        try:
            return json.loads(PROGRESS.read_text(encoding="utf-8"))
        except Exception:
            log("progress file corrupted, starting fresh")
    return {}


def seed_progress_from_db(progress: dict) -> int:
    """Pre-fill progress with (movie_id, vibe) pairs already having ≥3
    variants in the DB. Idempotent. Returns number of pairs newly added.
    Lets us add new movies/vibes without re-running already-done work after
    the on-disk progress.json is lost."""
    import sqlite3
    db_path = ROOT / "data" / "rewire.db"
    if not db_path.exists():
        return 0
    added = 0
    try:
        con = sqlite3.connect(str(db_path))
        cur = con.cursor()
        cur.execute("""
            SELECT movie_id, vibe, COUNT(*) FROM vibe_endings
            GROUP BY movie_id, vibe HAVING COUNT(*) >= 3
        """)
        for movie_id, vibe, _ in cur.fetchall():
            key = f"{movie_id}|{vibe}"
            if not progress.get(key, {}).get("ok"):
                progress[key] = {"ok": True, "model": "preexisting", "ts": int(time.time())}
                added += 1
        con.close()
    except Exception as e:
        log(f"seed_progress_from_db skipped: {e}")
    if added:
        save_progress(progress)
    return added


def save_progress(p: dict) -> None:
    with prog_lock:
        tmp = PROGRESS.with_suffix(".tmp")
        tmp.write_text(json.dumps(p, indent=2, sort_keys=True), encoding="utf-8")
        tmp.replace(PROGRESS)


def call_god(prompt: str, model: str, attempt: int = 0) -> str | None:
    body = {"prompt": prompt, "model": model, "format": "json"}
    timeout = 120
    try:
        r = requests.post(
            GOD_URL,
            json=body,
            headers={"Authorization": f"Bearer {GOD_TOKEN}",
                     "Content-Type": "application/json"},
            verify=False,
            timeout=timeout,
        )
        if r.status_code != 200:
            log(f"god HTTP {r.status_code} attempt {attempt}: {r.text[:160]}")
            return None
        j = r.json()
        if not j.get("success"):
            log(f"god !success attempt {attempt}: {str(j)[:160]}")
            return None
        return j.get("response", "")
    except requests.exceptions.RequestException as e:
        log(f"god exception attempt {attempt}: {e}")
        return None


def _coerce_ending(x) -> str:
    """An ending row may come back as a string OR a dict like
    {"text": "..."} or {"ending": "..."}; normalise to a plain string."""
    if isinstance(x, str):
        return x.strip()
    if isinstance(x, dict):
        for k in ("text", "ending", "phrase", "sentence", "value"):
            v = x.get(k)
            if isinstance(v, str) and v.strip():
                return v.strip()
        # Fall back to the first string-valued field.
        for v in x.values():
            if isinstance(v, str) and v.strip():
                return v.strip()
    return ""


def parse_endings(payload, depth: int = 0) -> list[str]:
    """Try strict JSON first, then a forgiving regex fallback. Accepts either
    a raw string OR an already-parsed dict/list (god may parse JSON for us
    when format=json). Recursive: peeks under god's `answer` wrapper."""
    if payload is None or depth > 4:
        return []
    if isinstance(payload, dict):
        # God wraps replies in {"answer": ...}. Peek inside first.
        for wrap in ("answer", "data", "result", "output"):
            if wrap in payload:
                inner = parse_endings(payload[wrap], depth + 1)
                if inner:
                    return inner
        for k in ("endings", "alternate_endings", "results", "items"):
            arr = payload.get(k)
            if isinstance(arr, list):
                out = [_coerce_ending(x) for x in arr]
                out = [x for x in out if x]
                if out:
                    return out
        # The whole dict might be {"1": "...", "2": "...", "3": "..."}.
        vals = list(payload.values())
        if vals and all(isinstance(v, (str, dict)) for v in vals):
            out = [_coerce_ending(v) for v in vals]
            out = [x for x in out if x]
            if out:
                return out
        return []
    if isinstance(payload, list):
        out = [_coerce_ending(x) for x in payload]
        return [x for x in out if x]
    text = str(payload).strip()
    if not text:
        return []
    text = re.sub(r"^```(?:json)?\s*|\s*```$", "", text, flags=re.MULTILINE).strip()
    try:
        j = json.loads(text)
        return parse_endings(j, depth + 1)
    except Exception:
        pass
    bits = re.split(r"\n+|(?<=[.!?])\s+", text)
    out = []
    for b in bits:
        b = re.sub(r"^\s*[\d\-\*\u2022\.]+\s*", "", b).strip(' "\'`')
        if b and len(b.split()) <= 18:
            if not b.endswith((".", "!", "?")):
                b += "."
            out.append(b)
    return out[:3]


def upsert_vibe(movie_id: str, vibe: str, variant: int, text, model: str) -> bool:
    text = _coerce_ending(text) if not isinstance(text, str) else text.strip().rstrip(',;')
    if not text:
        return False
    body = {
        "movie_id": movie_id, "vibe": vibe,
        "variant": variant, "text": text, "model": model,
    }
    try:
        r = requests.post(
            f"{REWIRE_BASE}/api/admin/vibe/upsert",
            json=body,
            headers={"X-Rewire-Admin": ADMIN_TOK,
                     "Content-Type": "application/json"},
            verify=False,
            timeout=30,
        )
        if r.status_code != 200:
            log(f"upsert {movie_id}/{vibe}/{variant} HTTP {r.status_code}: {r.text[:120]}")
            return False
        return True
    except requests.exceptions.RequestException as e:
        log(f"upsert {movie_id}/{vibe}/{variant} exception: {e}")
        return False


def process_movie_vibe(movie: dict, vibe: str, progress: dict) -> tuple[str, str, bool, str]:
    try:
        return _process_inner(movie, vibe, progress)
    except Exception as e:
        import traceback
        log(f"EXC {movie.get('id')}/{vibe}: {e}\n{traceback.format_exc()[:600]}")
        return (movie.get("id", "?"), vibe, False, f"exc:{type(e).__name__}")


def _process_inner(movie: dict, vibe: str, progress: dict) -> tuple[str, str, bool, str]:
    mid = movie["id"]
    key = f"{mid}|{vibe}"
    if progress.get(key, {}).get("ok"):
        return (mid, vibe, True, "skip")
    prompt = PROMPT_TMPL.format(
        title=movie.get("title", mid),
        year=movie.get("year", ""),
        genre=movie.get("genre", ""),
        synopsis=movie.get("synopsis", "")[:400],
        actual=movie.get("actual_ending", "")[:300],
        vibe_label=VIBE_LABEL[vibe],
        vibe_hint=VIBE_HINTS[vibe],
    )
    last_err = ""
    for attempt in range(3):
        model = MODELS[(hash(key) + attempt) % len(MODELS)]
        resp = call_god(prompt, model, attempt)
        if not resp:
            last_err = "no response"
            time.sleep(2 ** attempt)
            continue
        endings = parse_endings(resp)
        if len(endings) < 3:
            last_err = f"parsed {len(endings)} endings"
            preview = (resp if isinstance(resp, str) else json.dumps(resp))[:200]
            log(f"{mid}/{vibe} attempt {attempt} bad parse raw: {preview}")
            time.sleep(1)
            continue
        # Persist exactly 3 variants. Trim each to ~16 words just in case.
        ok_count = 0
        for v in range(3):
            ending = endings[v]
            words = ending.split()
            if len(words) > 16:
                ending = " ".join(words[:16]).rstrip('.,;') + "."
            if upsert_vibe(mid, vibe, v + 1, ending, model):
                ok_count += 1
        if ok_count == 3:
            with prog_lock:
                progress[key] = {"ok": True, "model": model, "ts": int(time.time())}
            save_progress(progress)
            return (mid, vibe, True, model)
        last_err = f"only {ok_count}/3 upserted"
        time.sleep(2)
    return (mid, vibe, False, last_err)


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--workers", type=int, default=4)
    ap.add_argument("--limit", type=int, default=0,
                    help="process at most N (movie,vibe) tuples (0=all)")
    ap.add_argument("--vibes", default=",".join(VIBES),
                    help="comma-separated subset of vibes to process")
    args = ap.parse_args()

    movies_file = ROOT / "data" / "movies.json"
    movies = json.loads(movies_file.read_text(encoding="utf-8"))
    log(f"loaded {len(movies)} movies from {movies_file}")
    progress = load_progress()
    pre_seeded = seed_progress_from_db(progress)
    if pre_seeded:
        log(f"pre-seeded progress with {pre_seeded} (movie,vibe) pairs already in DB")
    vibes = [v.strip() for v in args.vibes.split(",") if v.strip() in VIBES]
    log(f"vibes: {vibes}")

    tuples = [(m, v) for m in movies for v in vibes]
    random.shuffle(tuples)
    total = len(tuples)
    pending = [t for t in tuples if not progress.get(f"{t[0]['id']}|{t[1]}", {}).get("ok")]
    log(f"total tuples: {total}, pending: {len(pending)}")

    if args.limit:
        pending = pending[:args.limit]
        log(f"limited to {len(pending)} this run")

    done = 0
    fails = 0
    started = time.time()
    with ThreadPoolExecutor(max_workers=args.workers) as ex:
        futs = {ex.submit(process_movie_vibe, m, v, progress): (m["id"], v)
                for (m, v) in pending}
        for fut in as_completed(futs):
            mid, vibe = futs[fut]
            try:
                _, _, ok, info = fut.result()
            except Exception as e:
                log(f"worker exception {mid}/{vibe}: {e}")
                ok, info = False, "exc"
            done += 1
            if not ok:
                fails += 1
            if done % 5 == 0 or not ok:
                elapsed = time.time() - started
                rate = done / max(1, elapsed)
                eta = (len(pending) - done) / max(0.001, rate)
                log(f"  {done}/{len(pending)} ({mid}/{vibe} -> {info})  fails={fails}  ETA {eta/60:.1f}min")

    log(f"DONE. processed={done}, fails={fails}")


if __name__ == "__main__":
    sys.exit(main() or 0)
