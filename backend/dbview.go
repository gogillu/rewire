// Package main — read-only SQLite browser for the Rewire admin UI.
//
// Mounts at /dbview (HTML page) and /api/dbview/* (JSON). Lets an
// authenticated admin list tables, inspect schemas, browse rows with
// pagination/sort/filter, and run ad-hoc SELECT queries.
//
// Defense in depth:
//   - All routes gated by s.adminAuth (cookie/header/?token=).
//   - `table` and `order_by` params validated against sqlite_master so
//     they cannot be injected into SQL strings.
//   - Custom SQL is whitelisted to SELECT / WITH / EXPLAIN, semicolons
//     stripped, and a list of write keywords blocked.
//   - A hard LIMIT is appended if the query has none.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ---------- HTTP handlers ----------

func (s *Server) handleDBViewPage(w http.ResponseWriter, r *http.Request) {
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
	_, _ = w.Write([]byte(dbViewHTML))
}

// GET /api/dbview/tables — every user table + view in the db, with
// approximate row counts. Counts are run with COUNT(*) which is fine on
// SQLite (the WAL file keeps everything snappy at this scale).
func (s *Server) handleDBViewTables(w http.ResponseWriter, r *http.Request) {
	if err := s.adminAuth(r); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT name, type FROM sqlite_master
		   WHERE type IN ('table','view')
		     AND name NOT LIKE 'sqlite_%'
		   ORDER BY type, name`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	type tinfo struct {
		Name string `json:"name"`
		Type string `json:"type"`
		Rows int64  `json:"rows"`
	}
	out := []tinfo{}
	for rows.Next() {
		var t tinfo
		if err := rows.Scan(&t.Name, &t.Type); err != nil {
			continue
		}
		// Identifier is whitelisted (came from sqlite_master), so quoting
		// it in double quotes is safe and avoids reserved-word clashes.
		_ = s.db.QueryRowContext(r.Context(),
			fmt.Sprintf(`SELECT COUNT(*) FROM "%s"`, escapeIdent(t.Name))).Scan(&t.Rows)
		out = append(out, t)
	}
	writeJSON(w, http.StatusOK, map[string]any{"tables": out})
}

// GET /api/dbview/schema?table=X — columns, indexes, and CREATE statement.
func (s *Server) handleDBViewSchema(w http.ResponseWriter, r *http.Request) {
	if err := s.adminAuth(r); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	table := strings.TrimSpace(r.URL.Query().Get("table"))
	if !s.isKnownTable(table) {
		http.Error(w, "unknown table", http.StatusBadRequest)
		return
	}

	cols := queryRowsArgs(s.db,
		fmt.Sprintf(`PRAGMA table_info("%s")`, escapeIdent(table)))
	idx := queryRowsArgs(s.db,
		fmt.Sprintf(`PRAGMA index_list("%s")`, escapeIdent(table)))
	fks := queryRowsArgs(s.db,
		fmt.Sprintf(`PRAGMA foreign_key_list("%s")`, escapeIdent(table)))

	var createSQL string
	_ = s.db.QueryRow(`SELECT COALESCE(sql,'') FROM sqlite_master WHERE name = ?`, table).
		Scan(&createSQL)

	writeJSON(w, http.StatusOK, map[string]any{
		"table":      table,
		"columns":    cols,
		"indexes":    idx,
		"foreign_keys": fks,
		"create_sql": createSQL,
	})
}

// GET /api/dbview/rows?table=X&limit=50&offset=0&order_by=col&dir=desc
//                     &q=substr&col=col_name
//
// Returns the paginated rows plus total row count for the (optionally
// filtered) view.
func (s *Server) handleDBViewRows(w http.ResponseWriter, r *http.Request) {
	if err := s.adminAuth(r); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	q := r.URL.Query()
	table := strings.TrimSpace(q.Get("table"))
	if !s.isKnownTable(table) {
		http.Error(w, "unknown table", http.StatusBadRequest)
		return
	}
	cols, err := s.tableColumns(table)
	if err != nil || len(cols) == 0 {
		http.Error(w, "table has no columns", http.StatusBadRequest)
		return
	}

	limit := atoiDefault(q.Get("limit"), 50)
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	offset := atoiDefault(q.Get("offset"), 0)
	if offset < 0 {
		offset = 0
	}

	orderBy := strings.TrimSpace(q.Get("order_by"))
	if orderBy != "" && !containsStr(cols, orderBy) {
		orderBy = ""
	}
	dir := strings.ToLower(strings.TrimSpace(q.Get("dir")))
	if dir != "asc" && dir != "desc" {
		dir = "asc"
	}

	filterCol := strings.TrimSpace(q.Get("col"))
	filterVal := q.Get("q")
	whereSQL := ""
	args := []any{}
	if filterVal != "" {
		if filterCol != "" && containsStr(cols, filterCol) {
			whereSQL = fmt.Sprintf(` WHERE CAST("%s" AS TEXT) LIKE ?`, escapeIdent(filterCol))
			args = append(args, "%"+filterVal+"%")
		} else {
			// Fallback: search across all text-castable columns.
			parts := make([]string, 0, len(cols))
			for _, c := range cols {
				parts = append(parts,
					fmt.Sprintf(`CAST("%s" AS TEXT) LIKE ?`, escapeIdent(c)))
				args = append(args, "%"+filterVal+"%")
			}
			whereSQL = " WHERE (" + strings.Join(parts, " OR ") + ")"
		}
	}

	// Total (filtered) count.
	var total int64
	countSQL := fmt.Sprintf(`SELECT COUNT(*) FROM "%s"%s`,
		escapeIdent(table), whereSQL)
	if err := s.db.QueryRowContext(r.Context(), countSQL, args...).Scan(&total); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	orderSQL := ""
	if orderBy != "" {
		orderSQL = fmt.Sprintf(` ORDER BY "%s" %s`, escapeIdent(orderBy), strings.ToUpper(dir))
	}
	dataSQL := fmt.Sprintf(`SELECT * FROM "%s"%s%s LIMIT ? OFFSET ?`,
		escapeIdent(table), whereSQL, orderSQL)
	dataArgs := append([]any{}, args...)
	dataArgs = append(dataArgs, limit, offset)

	rows, err := s.db.QueryContext(r.Context(), dataSQL, dataArgs...)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	rcols, _ := rows.Columns()
	out := make([][]any, 0, limit)
	for rows.Next() {
		ptrs := make([]any, len(rcols))
		vals := make([]any, len(rcols))
		for i := range ptrs {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			continue
		}
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				vals[i] = string(b)
			}
		}
		out = append(out, vals)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"table":   table,
		"columns": rcols,
		"rows":    out,
		"total":   total,
		"limit":   limit,
		"offset":  offset,
	})
}

// POST /api/dbview/query  body: {"sql": "SELECT ..."}
func (s *Server) handleDBViewQuery(w http.ResponseWriter, r *http.Request) {
	if err := s.adminAuth(r); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	var body struct {
		SQL   string `json:"sql"`
		Limit int    `json:"limit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	stmt, err := sanitizeReadOnlySQL(body.SQL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	limit := body.Limit
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	stmt = appendLimitIfMissing(stmt, limit)

	t0 := time.Now()
	rows, err := s.db.QueryContext(r.Context(), stmt)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer rows.Close()

	cols, _ := rows.Columns()
	out := make([][]any, 0, 64)
	n := 0
	for rows.Next() {
		ptrs := make([]any, len(cols))
		vals := make([]any, len(cols))
		for i := range ptrs {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			continue
		}
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				vals[i] = string(b)
			}
		}
		out = append(out, vals)
		n++
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"columns":    cols,
		"rows":       out,
		"row_count":  n,
		"elapsed_ms": time.Since(t0).Milliseconds(),
		"sql":        stmt,
	})
}

// ---------- helpers ----------

func atoiDefault(s string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return v
	}
	return def
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// escapeIdent escapes embedded double quotes inside an identifier so it can
// be safely placed between "..." in a SQL string. The caller is responsible
// for whitelisting the identifier first; this is just defense in depth.
func escapeIdent(s string) string {
	return strings.ReplaceAll(s, `"`, `""`)
}

func (s *Server) isKnownTable(name string) bool {
	if name == "" || strings.HasPrefix(strings.ToLower(name), "sqlite_") {
		return false
	}
	var n int
	_ = s.db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master
		   WHERE type IN ('table','view') AND name = ?`, name).Scan(&n)
	return n > 0
}

func (s *Server) tableColumns(table string) ([]string, error) {
	rows, err := s.db.Query(fmt.Sprintf(`PRAGMA table_info("%s")`, escapeIdent(table)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			continue
		}
		out = append(out, name)
	}
	return out, nil
}

// sanitizeReadOnlySQL accepts only single-statement SELECT / WITH / EXPLAIN
// queries with no write-side-effect keywords. It strips trailing semicolons
// and rejects anything else.
var writeKeywordRe = regexp.MustCompile(
	`(?i)\b(insert|update|delete|drop|alter|attach|detach|replace|vacuum|reindex|create|truncate|pragma|exec)\b`)

func sanitizeReadOnlySQL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("empty sql")
	}
	// Strip optional trailing semicolons.
	s = strings.TrimRight(s, "; \t\r\n")
	// Reject any internal semicolons (multiple statements).
	if strings.Contains(s, ";") {
		return "", errors.New("only one statement allowed (no `;`)")
	}
	low := strings.ToLower(s)
	switch {
	case strings.HasPrefix(low, "select"),
		strings.HasPrefix(low, "with"),
		strings.HasPrefix(low, "explain"):
		// ok
	default:
		return "", errors.New("only SELECT / WITH / EXPLAIN queries are allowed")
	}
	if writeKeywordRe.MatchString(low) {
		return "", errors.New("query contains a write keyword (insert/update/delete/...)")
	}
	return s, nil
}

func appendLimitIfMissing(s string, limit int) string {
	low := strings.ToLower(s)
	// Crude check is good enough — if the user already wrote LIMIT, leave it.
	if strings.Contains(low, " limit ") || strings.HasPrefix(low, "explain") {
		return s
	}
	return s + " LIMIT " + strconv.Itoa(limit)
}

// ---------- inline DB browser HTML ----------

const dbViewHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Rewire — DB browser</title>
<style>
:root{color-scheme:dark;--bg:#0a0a0c;--card:#15151a;--card2:#101014;--muted:#8a8a96;--fg:#fff;--accent:#ff007a;--accent2:#ff8a00;--border:#232328}
*{box-sizing:border-box}
html,body{margin:0;height:100%}
body{background:var(--bg);color:var(--fg);font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;}
header{display:flex;align-items:center;gap:14px;padding:14px 18px;border-bottom:1px solid var(--border);background:var(--card2);position:sticky;top:0;z-index:5}
header h1{margin:0;font-size:18px;letter-spacing:.5px}
header .tag{font-size:10px;color:var(--muted);letter-spacing:1.4px;text-transform:uppercase;background:#1d1d22;padding:3px 8px;border-radius:999px}
header a{color:var(--muted);text-decoration:none;font-size:12px;letter-spacing:.5px}
header a:hover{color:#fff}
.layout{display:grid;grid-template-columns:260px 1fr;height:calc(100vh - 53px)}
@media(max-width:760px){.layout{grid-template-columns:1fr;height:auto}}
aside{border-right:1px solid var(--border);background:var(--card2);overflow:auto}
aside .group{padding:14px 14px 6px;font-size:10px;color:var(--muted);text-transform:uppercase;letter-spacing:1.4px;font-weight:600;display:flex;justify-content:space-between;align-items:center}
aside button.t{
  display:flex;width:100%;justify-content:space-between;align-items:center;
  background:transparent;color:#ddd;border:0;padding:8px 14px;cursor:pointer;
  font:inherit;font-size:13px;text-align:left;border-left:3px solid transparent;
}
aside button.t:hover{background:#1a1a20}
aside button.t.active{background:#1d1d22;border-left-color:var(--accent);color:#fff;font-weight:600}
aside button.t .n{font-size:11px;color:var(--muted);font-variant-numeric:tabular-nums}
main{overflow:auto;padding:18px}
.tabs{display:flex;gap:4px;border-bottom:1px solid var(--border);margin-bottom:14px}
.tabs button{background:transparent;color:var(--muted);border:0;padding:10px 16px;cursor:pointer;font:inherit;font-size:13px;letter-spacing:.5px;border-bottom:2px solid transparent}
.tabs button.active{color:#fff;border-bottom-color:var(--accent)}
.toolbar{display:flex;gap:8px;flex-wrap:wrap;margin-bottom:12px;align-items:center}
input,select,textarea{background:#1d1d22;color:#fff;border:1px solid #2a2a30;padding:8px 10px;border-radius:8px;font:inherit;font-size:13px}
input:focus,select:focus,textarea:focus{outline:none;border-color:var(--accent)}
.btn{background:var(--accent);color:#fff;border:0;padding:8px 14px;font-weight:700;border-radius:8px;cursor:pointer;letter-spacing:.5px;font-size:12px}
.btn.secondary{background:#1d1d22;color:#fff;border:1px solid #2a2a30;font-weight:500}
.btn:disabled{opacity:.5;cursor:wait}
.muted{color:var(--muted)}
.err{color:#ff7878;background:#311515;border:1px solid #5a1a1a;padding:10px 12px;border-radius:8px;margin-bottom:12px;font-size:13px;white-space:pre-wrap}
table{width:100%;border-collapse:collapse;background:var(--card);border:1px solid var(--border);border-radius:10px;overflow:hidden;font-size:12px}
th,td{padding:7px 10px;text-align:left;border-bottom:1px solid #1d1d22;vertical-align:top}
th{font-size:10px;text-transform:uppercase;letter-spacing:1.2px;color:var(--muted);font-weight:600;background:var(--card2);position:sticky;top:0;cursor:pointer;user-select:none;white-space:nowrap}
th .arrow{margin-left:4px;color:var(--accent)}
tr:hover td{background:#19191e}
td{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;max-width:380px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
td.json{cursor:pointer}
.null{color:#5a5a64;font-style:italic}
.scroll{overflow:auto;max-height:calc(100vh - 240px);border-radius:10px;border:1px solid var(--border)}
.scroll table{border:0;border-radius:0}
pre{background:#0d0d10;border:1px solid var(--border);border-radius:8px;padding:12px;overflow:auto;font-size:12px}
textarea{width:100%;min-height:120px;font-family:ui-monospace,SFMono-Regular,Menlo,monospace}
.gate{max-width:420px;margin:80px auto;padding:24px;background:var(--card);border-radius:16px;border:1px solid var(--border)}
.row{display:flex;gap:10px;align-items:center;flex-wrap:wrap}
small{font-size:11px;color:var(--muted)}
kbd{background:#1d1d22;border:1px solid #2a2a30;border-radius:5px;padding:1px 6px;font-size:11px;font-family:ui-monospace,monospace}
.modal{position:fixed;inset:0;background:rgba(0,0,0,.7);display:none;align-items:center;justify-content:center;z-index:99}
.modal.show{display:flex}
.modal .box{background:var(--card);border:1px solid var(--border);border-radius:14px;max-width:880px;width:90vw;max-height:80vh;overflow:auto;padding:18px}
.modal pre{margin:0;white-space:pre-wrap;word-break:break-word}
</style>
</head>
<body>
<header>
  <h1>Rewire <span class="muted" style="font-weight:400">DB browser</span></h1>
  <span class="tag">data/rewire.db</span>
  <span style="flex:1"></span>
  <a href="/dashboard">→ dashboard</a>
</header>

<div id="gate" class="gate" style="display:none">
  <h2 style="margin-top:0">DB browser</h2>
  <p class="muted">Enter the admin token to continue.</p>
  <div class="row" style="margin-top:14px">
    <input id="tokIn" type="password" placeholder="admin token" autocomplete="off" style="flex:1">
    <button class="btn" id="tokBtn">Open</button>
  </div>
  <p style="margin-top:16px"><small>Stored in a HttpOnly cookie for 30 days.</small></p>
</div>

<div class="layout" id="app" style="display:none">
  <aside>
    <div class="group">Tables <a href="#" id="refresh" class="muted" style="text-transform:none;letter-spacing:0;font-size:11px">refresh</a></div>
    <div id="tableList"></div>
    <div class="group">Views</div>
    <div id="viewList"></div>
  </aside>
  <main>
    <div id="empty" class="muted" style="margin-top:40px;text-align:center">
      <p style="font-size:14px">Pick a table on the left to browse it.</p>
      <p><small>Or open the <kbd>SQL</kbd> tab to run a SELECT query.</small></p>
      <p style="margin-top:30px">
        <button class="btn secondary" onclick="showQueryTab()">Open SQL console</button>
      </p>
    </div>
    <div id="viewer" style="display:none">
      <h2 id="title" style="margin:0 0 6px;font-size:16px"></h2>
      <div class="muted" id="subtitle" style="font-size:11px;letter-spacing:1px;text-transform:uppercase;margin-bottom:14px"></div>
      <div class="tabs">
        <button data-tab="rows" class="active">Rows</button>
        <button data-tab="schema">Schema</button>
        <button data-tab="sql">SQL</button>
      </div>
      <div id="errBox"></div>
      <div data-pane="rows">
        <div class="toolbar">
          <select id="filterCol"><option value="">(any column)</option></select>
          <input id="filterQ" placeholder="filter… LIKE %text%" style="flex:1;min-width:180px">
          <button class="btn secondary" id="applyFilter">Filter</button>
          <span style="flex:1"></span>
          <button class="btn secondary" id="prevPage">‹ prev</button>
          <span class="muted" id="pageInfo">—</span>
          <button class="btn secondary" id="nextPage">next ›</button>
          <select id="pageSize">
            <option>25</option><option selected>50</option><option>100</option><option>250</option>
          </select>
        </div>
        <div class="scroll"><table id="rowsTable"></table></div>
      </div>
      <div data-pane="schema" style="display:none">
        <h3 style="margin-top:0">Columns</h3>
        <div class="scroll"><table id="schemaTable"></table></div>
        <h3>Indexes</h3>
        <div class="scroll"><table id="indexTable"></table></div>
        <h3>Foreign keys</h3>
        <div class="scroll"><table id="fkTable"></table></div>
        <h3>CREATE statement</h3>
        <pre id="createSQL"></pre>
      </div>
      <div data-pane="sql" style="display:none">
        <p class="muted" style="font-size:12px;margin:0 0 8px">
          Read-only. Only <kbd>SELECT</kbd> / <kbd>WITH</kbd> / <kbd>EXPLAIN</kbd> are accepted. <kbd>Ctrl+Enter</kbd> to run.
        </p>
        <textarea id="sqlBox" spellcheck="false"></textarea>
        <div class="toolbar" style="margin-top:8px">
          <button class="btn" id="runSQL">Run query</button>
          <span class="muted" id="sqlMeta"></span>
        </div>
        <div class="scroll" style="margin-top:10px"><table id="sqlTable"></table></div>
      </div>
    </div>
  </main>
</div>

<div id="cellModal" class="modal" onclick="if(event.target===this)this.classList.remove('show')">
  <div class="box">
    <div class="row" style="margin-bottom:10px">
      <strong id="cellTitle">cell</strong>
      <span style="flex:1"></span>
      <button class="btn secondary" onclick="document.getElementById('cellModal').classList.remove('show')">close</button>
    </div>
    <pre id="cellBody"></pre>
  </div>
</div>

<script>
const $ = s => document.querySelector(s);
const $$ = s => Array.from(document.querySelectorAll(s));

let state = {
  tables: [],
  current: null, // table name
  cols: [],
  orderBy: '',
  dir: 'asc',
  offset: 0,
  limit: 50,
  filterCol: '',
  filterQ: '',
};

// We deliberately do NOT read the rewire_admin cookie from JS — it's
// HttpOnly when set by /dbview?token=, so document.cookie can't see it.
// Instead we probe an API endpoint: the cookie is always sent with
// same-origin requests, so a 200 means we're authenticated.
let manualToken = '';
function authHdr(){
  return manualToken ? {'X-Rewire-Admin': manualToken} : {};
}

async function api(path, opts){
  const r = await fetch(path, Object.assign({headers: authHdr(), credentials: 'same-origin'}, opts||{}));
  if(r.status === 403){ manualToken=''; gate(); throw new Error('forbidden'); }
  if(!r.ok){ throw new Error((await r.text()) || ('http '+r.status)); }
  return await r.json();
}

function gate(){
  $('#gate').style.display='block';
  $('#app').style.display='none';
  $('#tokIn').focus();
}
function ungate(){
  $('#gate').style.display='none';
  $('#app').style.display='grid';
  loadTables();
}

$('#tokBtn').onclick = async () => {
  const t = $('#tokIn').value.trim();
  if(!t) return;
  manualToken = t;
  // Probe to confirm token is good before ungating.
  try{
    await api('/api/dbview/tables');
    ungate();
  } catch(e){ alert('Invalid token'); }
};
$('#tokIn').addEventListener('keydown', e => { if(e.key==='Enter') $('#tokBtn').click(); });

// On load: if /dbview?token=X just set the cookie server-side, the API
// probe will succeed without us knowing the token. Otherwise show gate.
(async function init(){
  // Strip token from address bar so it doesn't sit in browser history.
  if(location.search.indexOf('token=') >= 0 && history.replaceState){
    history.replaceState({}, '', location.pathname);
  }
  try{
    await api('/api/dbview/tables');
    ungate();
  } catch(e){ gate(); }
})();

$('#refresh').onclick = (e) => { e.preventDefault(); loadTables(); };

async function loadTables(){
  try{
    const r = await api('/api/dbview/tables');
    state.tables = r.tables || [];
    renderSidebar();
  }catch(err){ console.error(err); }
}

function renderSidebar(){
  const tables = state.tables.filter(t => t.type === 'table');
  const views = state.tables.filter(t => t.type === 'view');
  const fmt = n => n==null ? '' : (+n).toLocaleString();
  const html = arr => arr.map(t => {
    const active = t.name === state.current ? ' active' : '';
    return '<button class="t' + active + '" data-name="' + escAttr(t.name) + '">' +
           '<span>' + escHTML(t.name) + '</span>' +
           '<span class="n">' + fmt(t.rows) + '</span></button>';
  }).join('');
  $('#tableList').innerHTML = html(tables) || '<small class="muted" style="padding:8px 14px;display:block">no tables</small>';
  $('#viewList').innerHTML = html(views) || '<small class="muted" style="padding:8px 14px;display:block">no views</small>';
  $$('#tableList button.t, #viewList button.t').forEach(b => {
    b.onclick = () => openTable(b.dataset.name);
  });
}

function escHTML(s){ return String(s==null?'':s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c])); }
function escAttr(s){ return escHTML(s); }

async function openTable(name){
  state.current = name;
  state.offset = 0;
  state.orderBy = '';
  state.dir = 'asc';
  state.filterCol = '';
  state.filterQ = '';
  renderSidebar();
  $('#empty').style.display='none';
  $('#viewer').style.display='block';
  $('#title').textContent = name;
  $('#errBox').innerHTML = '';
  switchTab('rows');
  await Promise.all([loadSchema(), loadRows()]);
}

function switchTab(name){
  $$('.tabs button').forEach(b => b.classList.toggle('active', b.dataset.tab === name));
  $$('main [data-pane]').forEach(p => p.style.display = p.dataset.pane === name ? '' : 'none');
}
$$('.tabs button').forEach(b => b.onclick = () => switchTab(b.dataset.tab));

function showQueryTab(){
  $('#empty').style.display='none';
  $('#viewer').style.display='block';
  $('#title').textContent = 'SQL console';
  $('#subtitle').textContent = 'read-only';
  switchTab('sql');
}

async function loadSchema(){
  try{
    const r = await api('/api/dbview/schema?table=' + encodeURIComponent(state.current));
    state.cols = (r.columns || []).map(c => c.name);
    // Subtitle.
    const pkCols = (r.columns||[]).filter(c => c.pk).map(c => c.name);
    $('#subtitle').textContent = state.cols.length + ' cols' +
      (pkCols.length ? ' · pk: ' + pkCols.join(',') : '');
    // Schema tab tables.
    $('#schemaTable').innerHTML = renderTable(
      ['cid','name','type','notnull','dflt_value','pk'],
      (r.columns||[]).map(c => [c.cid, c.name, c.type, c.notnull, c.dflt_value, c.pk]));
    $('#indexTable').innerHTML = renderTable(
      ['seq','name','unique','origin','partial'],
      (r.indexes||[]).map(c => [c.seq, c.name, c.unique, c.origin, c.partial]));
    $('#fkTable').innerHTML = renderTable(
      ['id','seq','table','from','to','on_update','on_delete','match'],
      (r.foreign_keys||[]).map(c => [c.id, c.seq, c.table, c.from, c.to, c.on_update, c.on_delete, c.match]));
    $('#createSQL').textContent = r.create_sql || '(none)';

    // Filter column dropdown.
    const sel = $('#filterCol');
    sel.innerHTML = '<option value="">(any column)</option>' +
      state.cols.map(c => '<option value="' + escAttr(c) + '">' + escHTML(c) + '</option>').join('');
  }catch(e){ showErr(e); }
}

async function loadRows(){
  try{
    const params = new URLSearchParams({
      table: state.current,
      limit: state.limit,
      offset: state.offset,
      order_by: state.orderBy || '',
      dir: state.dir,
      col: state.filterCol || '',
      q: state.filterQ || '',
    });
    const r = await api('/api/dbview/rows?' + params.toString());
    renderRows(r);
  }catch(e){ showErr(e); }
}

function renderRows(r){
  const cols = r.columns || [];
  const rows = r.rows || [];
  let html = '<thead><tr>';
  for(const c of cols){
    const arrow = c === state.orderBy ? '<span class="arrow">' + (state.dir==='asc'?'▲':'▼') + '</span>' : '';
    html += '<th data-col="' + escAttr(c) + '">' + escHTML(c) + arrow + '</th>';
  }
  html += '</tr></thead><tbody>';
  if(rows.length === 0){
    html += '<tr><td colspan="' + cols.length + '" class="muted" style="text-align:center">(no rows)</td></tr>';
  } else {
    for(const row of rows){
      html += '<tr>';
      for(let i=0;i<cols.length;i++){
        html += renderCell(row[i]);
      }
      html += '</tr>';
    }
  }
  html += '</tbody>';
  $('#rowsTable').innerHTML = html;
  // Sort header click.
  $$('#rowsTable thead th').forEach(th => th.onclick = () => {
    const c = th.dataset.col;
    if(state.orderBy === c){ state.dir = state.dir === 'asc' ? 'desc' : 'asc'; }
    else{ state.orderBy = c; state.dir = 'asc'; }
    state.offset = 0;
    loadRows();
  });
  // Cell click for full value.
  $$('#rowsTable tbody td').forEach(td => td.onclick = () => openCell(td));

  const from = r.total === 0 ? 0 : (r.offset + 1);
  const to = Math.min(r.offset + rows.length, r.total);
  $('#pageInfo').textContent = from + '–' + to + ' of ' + (r.total||0).toLocaleString();
}

function renderCell(v){
  if(v == null) return '<td class="null">NULL</td>';
  let s;
  if(typeof v === 'object'){ s = JSON.stringify(v); }
  else { s = String(v); }
  const cls = (s.length > 80 || s.indexOf('\n') >= 0) ? ' class="json"' : '';
  return '<td' + cls + ' title="' + escAttr(s.slice(0, 200)) + '">' + escHTML(s) + '</td>';
}

function openCell(td){
  const txt = td.textContent;
  if(txt.length < 60 && txt.indexOf('\n') < 0) return;
  $('#cellTitle').textContent = 'value (' + txt.length + ' chars)';
  // Pretty print JSON if possible.
  try{
    const j = JSON.parse(txt);
    $('#cellBody').textContent = JSON.stringify(j, null, 2);
  } catch(e){
    $('#cellBody').textContent = txt;
  }
  $('#cellModal').classList.add('show');
}

function renderTable(cols, rows){
  let h = '<thead><tr>' + cols.map(c => '<th>' + escHTML(c) + '</th>').join('') + '</tr></thead><tbody>';
  if(!rows.length){ h += '<tr><td colspan="' + cols.length + '" class="muted" style="text-align:center">—</td></tr>'; }
  for(const r of rows){
    h += '<tr>' + r.map(v => renderCell(v)).join('') + '</tr>';
  }
  h += '</tbody>';
  return h;
}

function showErr(e){
  $('#errBox').innerHTML = '<div class="err">' + escHTML(e.message || String(e)) + '</div>';
}

$('#applyFilter').onclick = () => {
  state.filterCol = $('#filterCol').value;
  state.filterQ = $('#filterQ').value;
  state.offset = 0;
  loadRows();
};
$('#filterQ').addEventListener('keydown', e => { if(e.key==='Enter') $('#applyFilter').click(); });
$('#prevPage').onclick = () => { state.offset = Math.max(0, state.offset - state.limit); loadRows(); };
$('#nextPage').onclick = () => { state.offset = state.offset + state.limit; loadRows(); };
$('#pageSize').onchange = (e) => { state.limit = +e.target.value; state.offset = 0; loadRows(); };

// SQL tab.
$('#runSQL').onclick = runSQL;
$('#sqlBox').addEventListener('keydown', e => {
  if((e.ctrlKey || e.metaKey) && e.key === 'Enter'){ e.preventDefault(); runSQL(); }
});

async function runSQL(){
  const sql = $('#sqlBox').value.trim();
  if(!sql) return;
  $('#runSQL').disabled = true;
  $('#errBox').innerHTML = '';
  $('#sqlMeta').textContent = 'running…';
  try{
    const r = await api('/api/dbview/query', {
      method: 'POST',
      headers: Object.assign({'Content-Type':'application/json'}, authHdr()),
      body: JSON.stringify({sql})
    });
    $('#sqlMeta').textContent = (r.row_count||0) + ' rows · ' + (r.elapsed_ms||0) + ' ms';
    let html = '<thead><tr>' + (r.columns||[]).map(c => '<th>' + escHTML(c) + '</th>').join('') + '</tr></thead><tbody>';
    if(!(r.rows||[]).length){ html += '<tr><td colspan="' + (r.columns||[]).length + '" class="muted" style="text-align:center">(no rows)</td></tr>'; }
    for(const row of (r.rows||[])){ html += '<tr>' + row.map(renderCell).join('') + '</tr>'; }
    html += '</tbody>';
    $('#sqlTable').innerHTML = html;
    $$('#sqlTable tbody td').forEach(td => td.onclick = () => openCell(td));
  }catch(e){
    $('#sqlMeta').textContent = '';
    showErr(e);
  } finally{
    $('#runSQL').disabled = false;
  }
}
</script>
</body>
</html>`
