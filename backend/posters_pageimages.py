"""last-mile poster fetch using MediaWiki query API (prop=pageimages),
which is more reliable than REST summary for older Bollywood pages."""

import sqlite3, urllib.request, urllib.parse, json, time

DB = r'C:\Users\arushi\Rewire\data\rewire.db'

# Hand-picked Wikipedia article titles for the 30 stragglers. Verified on
# en.wikipedia.org. Year disambig where needed.
TITLES = {
    'gully-boy':            'Gully Boy',
    'lage-raho-munna-bhai': 'Lage Raho Munna Bhai',
    'dil-chahta-hai':       'Dil Chahta Hai',
    'kal-ho-naa-ho':        'Kal Ho Naa Ho',
    'rocket-singh':         'Rocket Singh: Salesman of the Year',
    'saathiya':             'Saathiya (2002 film)',
    'stree':                'Stree (2018 film)',
    'bareilly-ki-barfi':    'Bareilly Ki Barfi',
    'dum-laga-ke-haisha':   'Dum Laga Ke Haisha',
    'vicky-donor':          'Vicky Donor',
    'piku':                 'Piku',
    'talvar':               'Talvar (film)',
    'oye-lucky':            'Oye Lucky! Lucky Oye!',
    'the-lunchbox':         'The Lunchbox',
    'kapoor-and-sons':      'Kapoor & Sons',
    'raazi':                'Raazi',
    'mardaani':             'Mardaani',
    'drishyam':             'Drishyam (2015 film)',
    'drishyam-2':           'Drishyam 2 (2022 film)',
    'jawan':                'Jawan (film)',
    'devdas-2002':          'Devdas (2002 Hindi film)',
    'dabangg':              'Dabangg',
    'andhera':              'Pad Man (film)',
    'munjya':               'Munjya',
    'laapataa-ladies':      'Laapataa Ladies',
    'shershaah':            'Shershaah',
    '83':                   '83 (film)',
    'karthik-calling-karthik': 'Karthik Calling Karthik',
    'inglourious-basterds': 'Inglourious Basterds',
    'mirzapur':             'Mirzapur (TV series)',
}

def query_pageimage(title):
    qs = urllib.parse.urlencode({
        'action':'query', 'titles': title, 'prop':'pageimages',
        'piprop':'original|thumbnail', 'pithumbsize':'500',
        'format':'json', 'redirects':'1',
    })
    url = 'https://en.wikipedia.org/w/api.php?' + qs
    req = urllib.request.Request(url, headers={'User-Agent':'rewire/0.1 (gogillu.live)'})
    try:
        with urllib.request.urlopen(req, timeout=10) as r:
            data = json.loads(r.read())
        pages = data.get('query',{}).get('pages',{})
        for _, p in pages.items():
            if 'original' in p: return p['original'].get('source')
            if 'thumbnail' in p: return p['thumbnail'].get('source')
    except Exception as e:
        print(f'  err {title}: {e}')
    return None

con = sqlite3.connect(DB)
cur = con.cursor()
ok=0; fail=0
for mid, title in TITLES.items():
    src = query_pageimage(title)
    if src:
        cur.execute('UPDATE movies SET poster_url = ? WHERE id = ?', (src, mid))
        con.commit()
        ok += 1
        print(f'  OK   {mid:30s} {src[:80]}')
    else:
        fail += 1
        print(f'  FAIL {mid:30s} ({title})')
    time.sleep(0.40)

print(f'\ndone: ok={ok} fail={fail}')
con.close()
