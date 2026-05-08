"""one-shot poster backfill — picks up where the Go backfill left off,
explicitly URL-encoding apostrophes and trying region/kind aware variants
plus a bare-title last resort. Updates rewire.db directly."""

import sqlite3, urllib.request, urllib.parse, json, time, sys

DB = r'C:\Users\arushi\Rewire\data\rewire.db'

def variants(title, year, region, kind):
    out = []
    if kind == 'tv':
        if year:
            out.append(f'{title} ({year} TV series)')
        if region == 'bollywood':
            out += [f'{title} (Indian TV series)', f'{title} (Hindi TV series)',
                    f'{title} (web series)', f'{title} (Indian web series)']
        elif region == 'hollywood':
            out += [f'{title} (American TV series)', f'{title} (British TV series)']
        else:
            out += [f'{title} (South Korean TV series)', f'{title} (Spanish TV series)',
                    f'{title} (German TV series)']
        out += [f'{title} (TV series)', title]
    else:
        if year:
            out.append(f'{title} ({year} film)')
        if region == 'bollywood':
            out += [f'{title} (Hindi film)', f'{title} (Indian film)']
        elif region == 'hollywood':
            out += [f'{title} (American film)', f'{title} (British film)']
        else:
            out += [f'{title} (South Korean film)', f'{title} (Spanish film)']
        out += [f'{title} (film)', title]
    seen = set(); uniq = []
    for v in out:
        if v not in seen:
            seen.add(v); uniq.append(v)
    return uniq

def fetch_thumb(v):
    path = urllib.parse.quote(v.replace(' ', '_'), safe='')
    url = 'https://en.wikipedia.org/api/rest_v1/page/summary/' + path
    try:
        req = urllib.request.Request(url, headers={'User-Agent':'rewire/0.1 (gogillu.live)'})
        with urllib.request.urlopen(req, timeout=8) as r:
            if r.status != 200: return None
            data = json.loads(r.read())
        desc = (data.get('description') or '').lower()
        if 'disambiguation' in desc:
            return None
        for k in ('originalimage','thumbnail'):
            src = data.get(k,{}).get('source')
            if src: return src
    except Exception:
        return None
    return None

con = sqlite3.connect(DB)
cur = con.cursor()
cur.execute("""SELECT id,title,year,COALESCE(region,'bollywood'),COALESCE(kind,'movie')
               FROM movies WHERE COALESCE(poster_url,'') IN ('','none')""")
todo = cur.fetchall()
print(f'pending: {len(todo)}')
ok=0; fail=0; failed_titles=[]
for i,(mid,title,year,region,kind) in enumerate(todo):
    found = None; used = None
    for v in variants(title, year, region, kind):
        t = fetch_thumb(v)
        if t:
            found = t; used = v; break
        time.sleep(0.50)
    if found:
        cur.execute('UPDATE movies SET poster_url = ? WHERE id = ?', (found, mid))
        con.commit()
        ok += 1
        print(f'  [{i+1:>3}/{len(todo)}] OK   {mid:30s} via "{used}"')
    else:
        fail += 1
        failed_titles.append((mid, title))
        print(f'  [{i+1:>3}/{len(todo)}] FAIL {mid:30s} ({title})')
    time.sleep(1.5)

print()
print(f'done: ok={ok} fail={fail}')
print('still failing:')
for f in failed_titles: print('  ',f)
con.close()
