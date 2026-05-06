// Package main — admin stats + dashboard HTML.
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"
)

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if err := s.adminAuth(r); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	mode := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("mode")))
	if mode != "direct" && mode != "abhinav" && mode != "sagar" {
		mode = "all"
	}
	// modeFilter inserts an AND clause for non-'all' modes; for 'all' it's
	// a no-op so all rows are counted.
	modeFilter := ""
	args := []any{}
	if mode != "all" {
		modeFilter = " AND mode = ? "
		args = append(args, mode)
	}
	// Some queries don't already have a WHERE clause; for those we use
	// modeWhere which is " WHERE mode = ? " or empty.
	modeWhere := ""
	if mode != "all" {
		modeWhere = " WHERE mode = ? "
	}

	out := map[string]any{"mode": mode}

	// Totals.
	var totalEvents, totalLikes, totalImpressions, totalFeedback int64
	var uniqUsers, uniqSessions, uniqMovies int64
	scanCount := func(q string, dest *int64) {
		if mode == "all" {
			_ = s.db.QueryRow(q).Scan(dest)
			return
		}
		// Insert mode predicate. We rely on simple WHERE / AND patterns.
		qq := q
		if strings.Contains(strings.ToUpper(q), " WHERE ") {
			qq = q + " AND mode = ?"
		} else {
			qq = q + " WHERE mode = ?"
		}
		_ = s.db.QueryRow(qq, mode).Scan(dest)
	}
	scanCount(`SELECT COUNT(*) FROM telemetry_events`, &totalEvents)
	scanCount(`SELECT COUNT(*) FROM telemetry_events WHERE event_type='like'`, &totalLikes)
	scanCount(`SELECT COUNT(*) FROM telemetry_events WHERE event_type='impression'`, &totalImpressions)
	scanCount(`SELECT COUNT(*) FROM feedbacks`, &totalFeedback)
	scanCount(`SELECT COUNT(DISTINCT anon_id) FROM telemetry_events`, &uniqUsers)
	scanCount(`SELECT COUNT(DISTINCT session_id) FROM telemetry_events`, &uniqSessions)
	scanCount(`SELECT COUNT(DISTINCT movie_id) FROM telemetry_events WHERE movie_id != ''`, &uniqMovies)

	out["totals"] = map[string]int64{
		"events":      totalEvents,
		"likes":       totalLikes,
		"impressions": totalImpressions,
		"feedbacks":   totalFeedback,
		"users":       uniqUsers,
		"sessions":    uniqSessions,
		"movies_seen": uniqMovies,
	}

	out["top_movies"] = queryRowsArgs(s.db, `
        SELECT
            te.movie_id        AS id,
            COALESCE(m.title, ac.title, te.movie_id) AS title,
            COUNT(*)           AS impressions,
            COALESCE(SUM(te.duration_ms), 0)/1000.0 AS dwell_s,
            COALESCE(AVG(NULLIF(te.duration_ms,0)),0)/1000.0 AS avg_dwell_s,
            (SELECT COUNT(*) FROM telemetry_events te2
                 WHERE te2.event_type='like' AND te2.movie_id = te.movie_id `+modeFilter+`) AS likes
        FROM telemetry_events te
        LEFT JOIN movies m ON m.id = te.movie_id
        LEFT JOIN abhinav_content ac ON ac.id = te.movie_id
        WHERE te.event_type='impression' AND te.movie_id != '' `+modeFilter+`
        GROUP BY te.movie_id
        ORDER BY impressions DESC
        LIMIT 25
    `, append(append([]any{}, args...), args...)...)

	out["ending_leaderboard"] = queryRows(s.db, `
        SELECT
            e.movie_id,
            m.title,
            e.id        AS ending_id,
            e.text      AS text,
            e.likes     AS db_likes,
            (SELECT COUNT(*) FROM telemetry_events
                 WHERE event_type='like' AND ending_id = e.id) AS event_likes
        FROM endings e
        JOIN movies m ON m.id = e.movie_id
        ORDER BY e.likes DESC
        LIMIT 30
    `)

	out["countries"] = queryRowsArgs(s.db, `
        SELECT
            COALESCE(NULLIF(country,''),'Unknown') AS country,
            COUNT(DISTINCT anon_id) AS users,
            COUNT(*)                AS events
        FROM telemetry_events `+modeWhere+`
        GROUP BY 1 ORDER BY users DESC LIMIT 20
    `, args...)

	out["devices"] = queryRowsArgs(s.db, `
        SELECT
            COALESCE(NULLIF(device,''),'unknown')  AS device,
            COALESCE(NULLIF(os,''),'unknown')      AS os,
            COALESCE(NULLIF(browser,''),'unknown') AS browser,
            COUNT(DISTINCT anon_id) AS users
        FROM telemetry_events `+modeWhere+`
        GROUP BY 1,2,3 ORDER BY users DESC LIMIT 20
    `, args...)

	out["recent_feedback"] = queryRowsArgs(s.db, `
        SELECT
            ts, kind, text, country, ip, mode,
            substr(anon_id,1,8) AS anon_short
        FROM feedbacks `+modeWhere+`
        ORDER BY ts DESC LIMIT 50
    `, args...)

	// Hourly impression count for the last 7 days.
	since := time.Now().Add(-7 * 24 * time.Hour).UnixMilli()
	hourlyArgs := []any{since}
	hourlyArgs = append(hourlyArgs, args...)
	out["hourly_traffic"] = queryRowsArgs(s.db, `
        SELECT
            (ts/3600000)*3600 AS hour_unix,
            COUNT(*)           AS impressions
        FROM telemetry_events
        WHERE event_type='impression' AND ts >= ? `+modeFilter+`
        GROUP BY 1 ORDER BY 1 ASC
    `, hourlyArgs...)

	out["recent_users"] = queryRowsArgs(s.db, `
        SELECT
            substr(anon_id,1,8) AS anon_short,
            MAX(ts) AS last_seen,
            MIN(ts) AS first_seen,
            COUNT(*) AS events,
            (SELECT country FROM telemetry_events te2 WHERE te2.anon_id = te.anon_id AND te2.country != '' LIMIT 1) AS country,
            (SELECT device  FROM telemetry_events te2 WHERE te2.anon_id = te.anon_id AND te2.device  != '' LIMIT 1) AS device,
            (SELECT browser FROM telemetry_events te2 WHERE te2.anon_id = te.anon_id AND te2.browser != '' LIMIT 1) AS browser,
            (SELECT mode    FROM telemetry_events te2 WHERE te2.anon_id = te.anon_id ORDER BY ts DESC LIMIT 1) AS mode
        FROM telemetry_events te `+modeWhere+`
        GROUP BY anon_id
        ORDER BY last_seen DESC LIMIT 30
    `, args...)

	out["audio_health"] = queryRowsArgs(s.db, `
        SELECT
            event_type,
            COUNT(*) AS n
        FROM telemetry_events
        WHERE event_type IN ('audio_on','audio_off','audio_error','audio_blocked') `+modeFilter+`
        GROUP BY event_type
    `, args...)

	// Per-mode split — always show, regardless of the mode filter.
	out["mode_breakdown"] = queryRows(s.db, `
        SELECT
            COALESCE(NULLIF(mode,''),'direct') AS mode,
            COUNT(*) AS events,
            COUNT(DISTINCT anon_id) AS users,
            COUNT(DISTINCT session_id) AS sessions,
            SUM(CASE WHEN event_type='like' THEN 1 ELSE 0 END) AS likes,
            SUM(CASE WHEN event_type='impression' THEN 1 ELSE 0 END) AS impressions
        FROM telemetry_events
        GROUP BY 1 ORDER BY events DESC
    `)

	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	// Allow without token if user is just loading the page; the embedded JS
	// will demand a token before fetching /api/stats. Otherwise auth is
	// required and the cookie is set for convenience.
	want := getenvDefault("REWIRE_ADMIN", "")
	if want == "" {
		http.Error(w, "REWIRE_ADMIN env var not set on server", http.StatusServiceUnavailable)
		return
	}
	if t := r.URL.Query().Get("token"); t == want {
		http.SetCookie(w, &http.Cookie{
			Name: "rewire_admin", Value: t, Path: "/",
			HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
			MaxAge: 60 * 60 * 24 * 30,
		})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(dashboardHTML))
}

// ---------- query helpers ----------

func queryRows(db *sql.DB, q string) []map[string]any {
	return queryRowsArgs(db, q)
}

func queryRowsArgs(db *sql.DB, q string, args ...any) []map[string]any {
	rows, err := db.Query(q, args...)
	if err != nil {
		return []map[string]any{{"error": err.Error()}}
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	out := []map[string]any{}
	for rows.Next() {
		ptrs := make([]any, len(cols))
		vals := make([]any, len(cols))
		for i := range ptrs {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			continue
		}
		row := map[string]any{}
		for i, c := range cols {
			v := vals[i]
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			row[c] = v
		}
		out = append(out, row)
	}
	return out
}

func init() {
	// Avoid unused-import lint when go vet runs in stripped builds.
	_ = json.Marshal
	_ = fmt.Sprintf
	_ = html.EscapeString
	_ = strings.Join
}

// ---------- inline dashboard HTML ----------
//
// Plain HTML + vanilla JS, ~12 KB. Renders summary cards, top-movies table,
// ending leaderboard, country list, device breakdown, recent feedback,
// and an SVG hourly-traffic sparkline. No external assets so the page works
// even when offline.

const dashboardHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Rewire — Dashboard</title>
<style>
:root{color-scheme:dark;--bg:#0a0a0c;--card:#15151a;--muted:#8a8a96;--fg:#fff;--accent:#ff007a;}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;padding:18px;}
h1{margin:0 0 4px;font-size:24px;letter-spacing:.5px}
h2{margin:30px 0 10px;font-size:15px;color:var(--muted);text-transform:uppercase;letter-spacing:1.2px;font-weight:600}
.tag{font-size:11px;color:var(--muted);letter-spacing:1.2px;text-transform:uppercase}
.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(170px,1fr));gap:12px;margin-top:12px}
.kpi{background:var(--card);border:1px solid #232328;border-radius:14px;padding:16px}
.kpi .v{font-size:30px;font-weight:800;letter-spacing:.5px;font-variant-numeric:tabular-nums}
.kpi .k{margin-top:4px;font-size:11px;color:var(--muted);text-transform:uppercase;letter-spacing:1.4px}
table{width:100%;border-collapse:collapse;background:var(--card);border-radius:14px;overflow:hidden;border:1px solid #232328}
th,td{padding:10px 12px;text-align:left;font-size:13px;border-bottom:1px solid #1d1d22}
th{font-size:11px;text-transform:uppercase;letter-spacing:1.2px;color:var(--muted);font-weight:600;background:#101014}
tr:last-child td{border-bottom:0}
td.num{font-variant-numeric:tabular-nums;text-align:right}
.bar{display:inline-block;height:8px;background:linear-gradient(90deg,#ff8a00,#ff007a);border-radius:4px;vertical-align:middle;margin-right:6px}
.btn{background:var(--accent);color:#fff;border:0;padding:8px 14px;font-weight:700;border-radius:10px;cursor:pointer;letter-spacing:.5px}
input{background:#1d1d22;color:#fff;border:1px solid #2a2a30;padding:8px 10px;border-radius:10px;width:100%;font:inherit}
.row{display:flex;gap:10px;align-items:center;flex-wrap:wrap}
.muted{color:var(--muted)}
.kind-stupid{color:#ff5e5e}
.kind-interesting{color:#7dffae}
.kind-custom{color:#9faaff}
.spark{display:block;width:100%;height:120px}
.flex2{display:grid;grid-template-columns:1fr 1fr;gap:18px}
@media(max-width:760px){.flex2{grid-template-columns:1fr}}
.gate{max-width:420px;margin:80px auto;padding:24px;background:var(--card);border-radius:16px;border:1px solid #232328}
small{font-size:11px;color:var(--muted)}
.modes{display:flex;gap:6px;margin:14px 0 4px;flex-wrap:wrap}
.modes button{
  background:#15151a;color:var(--muted);
  border:1px solid #2a2a30;padding:6px 14px;border-radius:999px;
  cursor:pointer;font-weight:600;font-size:12px;letter-spacing:.4px;text-transform:uppercase;
}
.modes button.active{background:linear-gradient(90deg,#ff007a,#ff8a00);color:#fff;border-color:transparent}
.kind-abhinav{color:#ffd35e}
.kind-sagar{color:#4ad6ff}
.mode-pill{font-size:10px;padding:1px 7px;border-radius:999px;background:#1d1d22;color:var(--muted);letter-spacing:1px;text-transform:uppercase;margin-left:6px}
.mode-pill.abhinav{background:linear-gradient(90deg,#ff007a,#ff8a00);color:#fff}
.mode-pill.sagar{background:linear-gradient(90deg,#0066ff,#00cfff);color:#fff}
</style>
</head>
<body>
<div id="gate" class="gate" style="display:none">
  <h1>Rewire dashboard</h1>
  <p class="muted">Enter the admin token to continue.</p>
  <div class="row" style="margin-top:14px">
    <input id="tokIn" type="password" placeholder="admin token" autocomplete="off">
    <button class="btn" id="tokBtn">Open</button>
  </div>
  <p style="margin-top:16px"><small>The token is stored in a HttpOnly cookie for 30 days.</small></p>
</div>
<div id="app" style="display:none">
  <h1>Rewire <span class="muted" style="font-weight:400">dashboard</span></h1>
  <div class="tag">Realtime telemetry · auto-refresh 30s</div>

  <div class="modes" id="modeBar">
    <button data-mode="all" class="active">All</button>
    <button data-mode="direct">Direct (/)</button>
    <button data-mode="abhinav">Abhinav (/abhinav)</button>
    <button data-mode="sagar">Sagar (/sagar)</button>
  </div>

  <h2>Mode breakdown</h2>
  <table id="modeTable"></table>

  <div class="grid" id="kpis"></div>

  <h2>Hourly traffic — last 7 days</h2>
  <svg class="spark" id="spark" viewBox="0 0 1000 120" preserveAspectRatio="none"></svg>

  <h2>Top movies (by impressions)</h2>
  <table id="topMovies"></table>

  <div class="flex2" style="margin-top:18px">
    <div>
      <h2>Ending leaderboard</h2>
      <table id="endings"></table>
    </div>
    <div>
      <h2>Recent feedback</h2>
      <table id="feedback"></table>
    </div>
  </div>

  <div class="flex2" style="margin-top:18px">
    <div>
      <h2>Countries</h2>
      <table id="countries"></table>
    </div>
    <div>
      <h2>Devices</h2>
      <table id="devices"></table>
    </div>
  </div>

  <h2>Recent users</h2>
  <table id="users"></table>

  <h2>Audio health</h2>
  <table id="audio"></table>

  <p class="muted" style="margin-top:30px"><small>Updated <span id="updated">—</span>. Token cleared by deleting the rewire_admin cookie.</small></p>
</div>

<script>
const $ = s => document.querySelector(s);
let currentMode = 'all';
function fmt(n){
  if(n==null||isNaN(n))return '—';
  n = +n;
  if(Math.abs(n)>=1e6) return (n/1e6).toFixed(1)+'M';
  if(Math.abs(n)>=1e3) return (n/1e3).toFixed(1)+'k';
  return n.toFixed(n%1?1:0);
}
function htmlescape(s){return String(s==null?'':s).replace(/[&<>"]/g,c=>({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;"}[c]))}
function timeAgo(ms){
  const d = Date.now() - +ms; if(!isFinite(d)) return '';
  const s = Math.round(d/1000);
  if(s<60) return s+'s';
  if(s<3600) return Math.round(s/60)+'m';
  if(s<86400) return Math.round(s/3600)+'h';
  return Math.round(s/86400)+'d';
}
function tbl(el, headers, rows){
  el.innerHTML = '<thead><tr>'+headers.map(h=>'<th>'+htmlescape(h)+'</th>').join('')+'</tr></thead><tbody>'+
    rows.map(r=>'<tr>'+r.map((c,i)=>'<td'+(typeof c==='number'?' class="num"':'')+'>'+(c==null?'—':c)+'</td>').join('')+'</tr>').join('')+
    '</tbody>';
}

async function load(){
  const r = await fetch('/api/stats?mode='+encodeURIComponent(currentMode),{credentials:'include'});
  if(r.status===403){ document.cookie='rewire_admin=;Max-Age=0;path=/'; gate(); return; }
  const j = await r.json();

  const t = j.totals||{};
  $('#kpis').innerHTML = [
    ['Users (devices)', fmt(t.users)],
    ['Sessions', fmt(t.sessions)],
    ['Impressions', fmt(t.impressions)],
    ['Likes', fmt(t.likes)],
    ['Movies seen', fmt(t.movies_seen)],
    ['Feedbacks', fmt(t.feedbacks)],
    ['Total events', fmt(t.events)],
  ].map(([k,v])=>'<div class="kpi"><div class="v">'+v+'</div><div class="k">'+k+'</div></div>').join('');

  // Sparkline.
  const h = j.hourly_traffic||[];
  const spark = $('#spark');
  if(h.length){
    const max = Math.max(...h.map(p=>+p.impressions||0)) || 1;
    const w = 1000, hgt = 120, pad=4;
    const pts = h.map((p,i)=>{
      const x = (i/(h.length-1||1))*(w-2*pad)+pad;
      const y = hgt - pad - ((+p.impressions||0)/max)*(hgt-2*pad-10);
      return x+','+y;
    }).join(' ');
    spark.innerHTML =
      '<defs><linearGradient id="g" x1="0" x2="0" y1="0" y2="1"><stop offset="0" stop-color="#ff007a" stop-opacity=".7"/><stop offset="1" stop-color="#ff007a" stop-opacity="0"/></linearGradient></defs>'+
      '<polyline points="'+pts+'" fill="none" stroke="#ff007a" stroke-width="2"/>'+
      '<polygon points="'+pad+','+(hgt-pad)+' '+pts+' '+(w-pad)+','+(hgt-pad)+'" fill="url(#g)"/>';
  } else {
    spark.innerHTML = '<text x="50%" y="50%" fill="#888" text-anchor="middle" font-size="12">No data yet</text>';
  }

  // Top movies.
  tbl($('#topMovies'),
    ['Movie','Impr.','Total dwell (s)','Avg dwell (s)','Likes','Like-rate'],
    (j.top_movies||[]).map(m=>{
      const impr = +m.impressions||0, likes=+m.likes||0;
      const rate = impr ? (likes*100/impr).toFixed(1)+'%' : '—';
      return [
        '<strong>'+htmlescape(m.title||m.id)+'</strong>',
        fmt(impr), fmt(m.dwell_s), fmt(m.avg_dwell_s), fmt(likes), rate
      ];
    }));

  // Ending leaderboard.
  tbl($('#endings'),
    ['Movie','Ending','DB likes','Event likes'],
    (j.ending_leaderboard||[]).map(e=>[
      '<strong>'+htmlescape(e.title||e.movie_id)+'</strong>',
      htmlescape(String(e.text||'').slice(0,90)),
      fmt(e.db_likes), fmt(e.event_likes)
    ]));

  // Recent feedback.
  tbl($('#feedback'),
    ['When','Mode','Kind','Note','Country'],
    (j.recent_feedback||[]).map(f=>[
      '<span class="muted">'+timeAgo(f.ts)+'</span>',
      '<span class="mode-pill '+((f.mode==='abhinav'||f.mode==='sagar')?f.mode:'')+'">'+htmlescape(f.mode||'direct')+'</span>',
      '<span class="kind-'+htmlescape(f.kind)+'">'+htmlescape(f.kind)+'</span>',
      htmlescape((f.text||'').slice(0,180)) || '<span class="muted">—</span>',
      htmlescape(f.country||'')
    ]));

  // Mode breakdown — independent of the active filter.
  tbl($('#modeTable'),
    ['Mode','Users','Sessions','Impressions','Likes','Events'],
    (j.mode_breakdown||[]).map(m=>[
      '<span class="mode-pill '+((m.mode==='abhinav'||m.mode==='sagar')?m.mode:'')+'">'+htmlescape(m.mode)+'</span>',
      fmt(m.users), fmt(m.sessions), fmt(m.impressions), fmt(m.likes), fmt(m.events)
    ]));

  // Countries.
  const cmax = Math.max(1, ...(j.countries||[]).map(c=>+c.users||0));
  tbl($('#countries'),
    ['Country','Users','Events',''],
    (j.countries||[]).map(c=>[
      htmlescape(c.country),
      fmt(c.users), fmt(c.events),
      '<span class="bar" style="width:'+((+c.users||0)*120/cmax)+'px"></span>'
    ]));

  // Devices.
  tbl($('#devices'),
    ['Device','OS','Browser','Users'],
    (j.devices||[]).map(d=>[
      htmlescape(d.device), htmlescape(d.os), htmlescape(d.browser), fmt(d.users)
    ]));

  // Users.
  tbl($('#users'),
    ['Anon','Mode','Last seen','Events','Country','Device','Browser'],
    (j.recent_users||[]).map(u=>[
      '<code>'+htmlescape(u.anon_short)+'</code>',
      '<span class="mode-pill '+((u.mode==='abhinav'||u.mode==='sagar')?u.mode:'')+'">'+htmlescape(u.mode||'direct')+'</span>',
      timeAgo(u.last_seen), fmt(u.events),
      htmlescape(u.country||''), htmlescape(u.device||''), htmlescape(u.browser||'')
    ]));

  // Audio.
  tbl($('#audio'),
    ['Event','Count'],
    (j.audio_health||[]).map(a=>[ htmlescape(a.event_type), fmt(a.n) ]));

  $('#updated').textContent = new Date().toLocaleTimeString();
}

function gate(){
  $('#gate').style.display='block'; $('#app').style.display='none';
  $('#tokBtn').onclick = ()=>{
    const v = $('#tokIn').value.trim(); if(!v) return;
    location.search = '?token='+encodeURIComponent(v);
  };
  $('#tokIn').onkeydown = e=>{ if(e.key==='Enter') $('#tokBtn').click(); };
}

(async()=>{
  // Try to load. If 403, show gate.
  try{
    const r = await fetch('/api/stats',{credentials:'include'});
    if(r.status===403){ gate(); return; }
    $('#app').style.display='block';
    // Mode toggle bar.
    document.querySelectorAll('#modeBar button').forEach(b=>{
      b.onclick = ()=>{
        currentMode = b.dataset.mode;
        document.querySelectorAll('#modeBar button').forEach(x=>x.classList.toggle('active', x===b));
        load();
      };
    });
    await load();
    setInterval(load, 30000);
  }catch(e){ gate(); }
})();
</script>
</body>
</html>`
