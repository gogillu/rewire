"""Rewire ending generator — drives god backend (3 models) over 97 movies × 3 slots = 291 calls.

Idempotent: skips slots that already exist in the DB. Safe to re-run after a
crash. Writes progress to a JSON file so we can monitor.

Models (3 different "latest" LLMs):
  slot 1  -> gpt-5.5
  slot 2  -> claude-opus-4.7
  slot 3  -> claude-sonnet-4.6

Each ending is asked for as <= 10 words, intense/cinematic, completely
different universe from the actual ending.
"""

from __future__ import annotations

import json
import os
import sqlite3
import sys
import time
import urllib.request
import urllib.error
import ssl
from concurrent.futures import ThreadPoolExecutor, as_completed
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent          # C:\Users\arushi\Rewire
DB_PATH = ROOT / "data" / "rewire.db"
RUNTIME = Path(r"C:\Users\arushi\.copilot\session-state\abd0700f-1772-4351-81ba-707e1cdfc3db\files\rewire-runtime")
PROGRESS_FILE = RUNTIME / "preprocess-progress.json"
LOG_FILE = RUNTIME / "preprocess.log"
GOD_TOKEN_FILE = RUNTIME.parent / "god-token.txt"
ADMIN_TOKEN_FILE = RUNTIME / "admin-token.txt"

GOD_URL = "https://localhost:9871/v1/invoke"
REWIRE_UPSERT = "https://localhost:9999/api/admin/upsert-ending"

MODELS = [
    ("gpt-5.5",          1),
    ("claude-opus-4.7",  2),
    ("claude-sonnet-4.6",3),
]

PROMPT_TEMPLATE = """You are a cinematic dreamer rewriting a beloved Bollywood movie's ending into a parallel universe.

Movie: {title} ({year})
Genre: {genre}
Synopsis: {synopsis}
ACTUAL ending: {actual_ending}

Write ONE alternate ending that:
- is COMPLETELY different from the actual ending (not a tweak — a parallel-universe reality)
- evokes intense feeling: amused, excited, emotional, eerie, gut-punching, or absurd-but-believable
- is at most TEN WORDS, written as a single phrase a viewer's brain would replay forever
- uses no quotes, no period at the end is fine, no hashtags, no emojis
- references the characters by name where it lands harder

Reply with ONLY the phrase. No preamble, no labels.
""".strip()

CTX = ssl.create_default_context()
CTX.check_hostname = False
CTX.verify_mode = ssl.CERT_NONE


def log(msg: str) -> None:
    line = f"[{time.strftime('%H:%M:%S')}] {msg}"
    print(line, flush=True)
    LOG_FILE.parent.mkdir(parents=True, exist_ok=True)
    with LOG_FILE.open("a", encoding="utf-8") as f:
        f.write(line + "\n")


def load_token(path: Path) -> str:
    return path.read_text(encoding="utf-8").strip()


def http_post_json(url: str, body: dict, headers: dict, timeout: float = 240.0) -> dict:
    raw = json.dumps(body).encode("utf-8")
    req = urllib.request.Request(url, data=raw, headers={"Content-Type": "application/json", **headers})
    try:
        with urllib.request.urlopen(req, timeout=timeout, context=CTX) as r:
            return json.loads(r.read().decode("utf-8", "replace"))
    except urllib.error.HTTPError as e:
        body = e.read().decode("utf-8", "replace")
        raise RuntimeError(f"HTTP {e.code} from {url}: {body}") from None


def call_god(token: str, model: str, prompt: str) -> str:
    """Returns the model's text response, sanitised."""
    body = {
        "prompt": prompt,
        "model": model,
        "actionablesContext": False,
        "mcpMode": "none",
        "permissionMode": "yolo",
        "skill": None,
        "responseFormat": "text",
        "timeoutMs": 180_000,
    }
    out = http_post_json(GOD_URL, body, {"Authorization": f"Bearer {token}"}, timeout=240)
    # god returns either {"response": "..."} (text) or {"response":{"answer":"..."}} (json mode)
    resp = out.get("response", "")
    if isinstance(resp, dict):
        # try common keys
        for k in ("answer", "text", "phrase", "ending", "content"):
            if k in resp and isinstance(resp[k], str):
                resp = resp[k]
                break
        else:
            resp = json.dumps(resp)
    text = str(resp).strip()
    # Strip leading/trailing quotes some models add despite instructions.
    text = text.strip().strip('"\u201c\u201d\u2018\u2019').strip()
    # Cap to 80 chars just in case.
    if len(text) > 120:
        text = text[:117].rstrip() + "..."
    return text


def upsert_ending(admin_token: str, movie_id: str, slot: int, model: str, text: str) -> None:
    body = {"movie_id": movie_id, "slot": slot, "model": model, "text": text}
    headers = {"X-Rewire-Admin": admin_token}
    http_post_json(REWIRE_UPSERT, body, headers, timeout=20)


def load_movies() -> list[dict]:
    movies_path = ROOT / "data" / "movies.json"
    return json.loads(movies_path.read_text(encoding="utf-8"))


def existing_slots() -> set[tuple[str, int]]:
    if not DB_PATH.exists():
        return set()
    con = sqlite3.connect(str(DB_PATH))
    try:
        rows = con.execute("SELECT movie_id, slot FROM endings").fetchall()
        return {(m, s) for m, s in rows}
    finally:
        con.close()


def write_progress(state: dict) -> None:
    PROGRESS_FILE.parent.mkdir(parents=True, exist_ok=True)
    tmp = PROGRESS_FILE.with_suffix(".tmp")
    tmp.write_text(json.dumps(state, indent=2), encoding="utf-8")
    tmp.replace(PROGRESS_FILE)


def generate_one(token: str, admin_token: str, m: dict, model: str, slot: int) -> tuple[str, int, str | None, str | None]:
    """Returns (movie_id, slot, text or None, error or None)."""
    prompt = PROMPT_TEMPLATE.format(
        title=m["title"], year=m["year"], genre=m["genre"],
        synopsis=m["synopsis"], actual_ending=m["actual_ending"],
    )
    try:
        text = call_god(token, model, prompt)
        if not text or len(text) < 5:
            return m["id"], slot, None, "empty/short response"
        upsert_ending(admin_token, m["id"], slot, model, text)
        return m["id"], slot, text, None
    except Exception as e:
        return m["id"], slot, None, str(e)[:200]


def main() -> int:
    token = load_token(GOD_TOKEN_FILE)
    admin_token = load_token(ADMIN_TOKEN_FILE)
    movies = load_movies()
    done = existing_slots()

    # Build the work list (skip already-done slots).
    work: list[tuple[dict, str, int]] = []
    for m in movies:
        for model, slot in MODELS:
            if (m["id"], slot) in done:
                continue
            work.append((m, model, slot))

    total = len(movies) * len(MODELS)
    already_done = total - len(work)
    log(f"start: {len(movies)} movies × {len(MODELS)} slots = {total}; "
        f"already done: {already_done}; to do: {len(work)}")

    state = {
        "started_at": time.strftime("%Y-%m-%dT%H:%M:%S"),
        "total": total,
        "skipped_existing": already_done,
        "completed": 0,
        "failed": 0,
        "errors": [],
        "in_flight": 0,
        "running": True,
    }
    write_progress(state)

    # Be polite to god backend — it allows ~10 concurrent. We use 4 to leave
    # headroom for the user's normal traffic.
    completed = 0
    failed = 0
    last_state_dump = 0.0
    with ThreadPoolExecutor(max_workers=4) as pool:
        futures = {
            pool.submit(generate_one, token, admin_token, m, model, slot): (m["id"], model, slot)
            for (m, model, slot) in work
        }
        state["in_flight"] = len(futures)
        write_progress(state)
        for fut in as_completed(futures):
            mid, model, slot = futures[fut]
            try:
                _, _, text, err = fut.result()
            except Exception as e:
                text, err = None, str(e)[:200]
            if text:
                completed += 1
                log(f"[{completed + already_done}/{total}] {mid} slot{slot} ({model}): {text}")
            else:
                failed += 1
                state["errors"].append({"movie": mid, "slot": slot, "model": model, "error": err})
                log(f"[!! ] {mid} slot{slot} ({model}) FAILED: {err}")
            state["completed"] = completed
            state["failed"] = failed
            state["in_flight"] = max(0, state["in_flight"] - 1)
            now = time.time()
            if now - last_state_dump > 1.0:
                write_progress(state)
                last_state_dump = now

    state["running"] = False
    state["finished_at"] = time.strftime("%Y-%m-%dT%H:%M:%S")
    write_progress(state)
    log(f"done: completed={completed} failed={failed}")
    return 0 if failed == 0 else 1


if __name__ == "__main__":
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        log("interrupted")
        sys.exit(130)
