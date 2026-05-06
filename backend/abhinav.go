// Package main — /abhinav variant.
//
// Beta-tester branch built around Abhinav's feedback:
//   - Excludes biopics from the deck
//   - Adds TV series (with endings) per his ask
//   - Lets users write their own ending
//   - Lets users rate community endings; AI score = simple weighted sum
//   - Author attribution on every ending
//   - Telemetry tagged with mode='abhinav' for separable analytics
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// IDs of biopics in the existing 97-movie set. These are filtered OUT of the
// /api/abhinav/movies response per Abhinav's feedback ("autobiography should
// be excluded — people cannot go away from reality").
var abhinavBiopicExclude = map[string]bool{
	"ms-dhoni":           true,
	"bhaag-milkha-bhaag": true,
	"manjhi":             true,
	"shershaah":          true,
	"12th-fail":          true,
	"83":                 true,
	"andhera":            true, // Pad Man
}

const abhinavSchema = `
CREATE TABLE IF NOT EXISTS abhinav_content (
    id          TEXT PRIMARY KEY,
    kind        TEXT NOT NULL DEFAULT 'series',
    title       TEXT NOT NULL,
    year        INTEGER,
    rating      REAL,
    genre       TEXT,
    synopsis    TEXT,
    actual_ending TEXT,
    poster_url  TEXT,
    sort_order  INTEGER DEFAULT 0
);

CREATE TABLE IF NOT EXISTS abhinav_endings (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    content_id  TEXT NOT NULL,
    slot        INTEGER NOT NULL,
    source      TEXT NOT NULL DEFAULT 'ai',  -- 'ai' | 'community'
    author      TEXT,                          -- handle for community endings
    text        TEXT NOT NULL,
    likes       INTEGER NOT NULL DEFAULT 0,
    rating_sum  REAL NOT NULL DEFAULT 0,
    rating_n    INTEGER NOT NULL DEFAULT 0,
    ai_score    REAL NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_ae_content ON abhinav_endings(content_id);
CREATE INDEX IF NOT EXISTS idx_ae_source  ON abhinav_endings(source);

CREATE TABLE IF NOT EXISTS abhinav_ratings (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    ending_id   INTEGER NOT NULL,
    target      TEXT NOT NULL,        -- 'series' (abhinav_endings) or 'movie' (endings)
    anon_id     TEXT NOT NULL,
    rating      INTEGER NOT NULL,     -- 1..5
    created_at  INTEGER NOT NULL,
    UNIQUE(ending_id, target, anon_id)
);
`

// migrateAddModeColumn idempotently adds the 'mode' column to telemetry_events
// and feedbacks. SQLite has no IF NOT EXISTS for ADD COLUMN so we swallow the
// duplicate-column error instead.
func (s *Server) migrateAddModeColumn() {
	for _, tbl := range []string{"telemetry_events", "feedbacks"} {
		_, err := s.db.Exec(fmt.Sprintf(
			`ALTER TABLE %s ADD COLUMN mode TEXT NOT NULL DEFAULT 'direct'`, tbl))
		if err != nil && !strings.Contains(err.Error(), "duplicate column") {
			// Column already exists — that's the expected path on warm boot.
			continue
		}
	}
}

// ---------- Seed TV series + their endings ----------

type seriesSeed struct {
	ID, Title, Genre, Synopsis, ActualEnding, PosterURL string
	Year                                                int
	Rating                                              float64
	Endings                                             [3]string
}

var abhinavSeries = []seriesSeed{
	{
		ID: "himym", Title: "How I Met Your Mother", Year: 2014, Rating: 8.3,
		Genre:        "Sitcom",
		Synopsis:     "Ted Mosby tells his kids the long, winding story of how he met their mother.",
		ActualEnding: "The mother dies of illness; Ted ends up with Robin in the present day, much to fans' fury.",
		PosterURL:    "https://upload.wikimedia.org/wikipedia/en/8/8c/HIMYMO.png",
		Endings: [3]string{
			"Tracy lives; Ted's kids never let him retell the story",
			"Barney becomes a single dad and Robin moves to Antarctica",
			"Marshall reveals he was the narrator the entire time",
		},
	},
	{
		ID: "stranger-things", Title: "Stranger Things", Year: 2016, Rating: 8.7,
		Genre:        "Sci-Fi/Horror",
		Synopsis:     "Hawkins kids fight monsters from the Upside Down across decades.",
		ActualEnding: "Vecna defeated; Eleven seals the gates and Hawkins begins to heal.",
		PosterURL:    "https://upload.wikimedia.org/wikipedia/en/3/38/Stranger_Things_logo.png",
		Endings: [3]string{
			"Eleven becomes the new Vecna and locks Mike in the Upside Down",
			"Hopper opens a diner where every dish is named after a missing kid",
			"Will turns out to be the architect of the entire Upside Down",
		},
	},
	{
		ID: "friends", Title: "Friends", Year: 2004, Rating: 8.9,
		Genre:        "Sitcom",
		Synopsis:     "Six twenty-somethings navigate New York life and love together.",
		ActualEnding: "Ross and Rachel reunite at the airport; everyone leaves their apartment for adulthood.",
		PosterURL:    "https://upload.wikimedia.org/wikipedia/en/c/cc/Friends_season_one_cast.jpg",
		Endings: [3]string{
			"Rachel gets on the plane; Ross marries Joey instead",
			"Phoebe reveals she was a CIA operative all along",
			"Chandler becomes a sad clown after Joey gets fame",
		},
	},
	{
		ID: "breaking-bad", Title: "Breaking Bad", Year: 2013, Rating: 9.5,
		Genre:        "Crime/Drama",
		Synopsis:     "A chemistry teacher cooks meth to secure his family's future and loses everything.",
		ActualEnding: "Walter dies in his lab after freeing Jesse; Skyler and the kids inherit nothing.",
		PosterURL:    "https://upload.wikimedia.org/wikipedia/en/6/61/Breaking_Bad_title_card.png",
		Endings: [3]string{
			"Walt fakes death and runs a chemistry coaching class in Jaipur",
			"Jesse becomes the new Heisenberg and franchises his blue meth",
			"Skyler turns out to have been the real kingpin all season",
		},
	},
	{
		ID: "game-of-thrones", Title: "Game of Thrones", Year: 2019, Rating: 9.2,
		Genre:        "Fantasy",
		Synopsis:     "Noble houses war for the Iron Throne while an undead army marches south.",
		ActualEnding: "Bran the Broken takes the throne; Jon Snow exiled north of the Wall.",
		PosterURL:    "https://upload.wikimedia.org/wikipedia/en/d/d8/Game_of_Thrones_title_card.jpg",
		Endings: [3]string{
			"Daenerys wins, exiles Jon and starts a tax-free zone",
			"The Night King opens a frozen-yogurt chain in King's Landing",
			"Arya kills the showrunners and writes the next 6 episodes",
		},
	},
	{
		ID: "the-office-us", Title: "The Office (US)", Year: 2013, Rating: 9.0,
		Genre:        "Sitcom/Mockumentary",
		Synopsis:     "Daily life of Dunder Mifflin's Scranton branch under managers who don't manage.",
		ActualEnding: "Jim and Pam move to Austin; Michael returns for Dwight's wedding.",
		PosterURL:    "https://upload.wikimedia.org/wikipedia/en/9/9f/The_Office_US_cast.jpg",
		Endings: [3]string{
			"Toby finally murders Michael Scott in the warehouse — quietly",
			"Dwight is revealed to be a real assistant to a real regional manager",
			"Stanley retires to the Bahamas and starts a beet farm",
		},
	},
	{
		ID: "money-heist", Title: "Money Heist (La Casa de Papel)", Year: 2021, Rating: 8.2,
		Genre:        "Heist",
		Synopsis:     "The Professor leads a multinational crew of thieves through two impossible heists.",
		ActualEnding: "Crew exfiltrates the gold; the Professor reunites with Raquel.",
		PosterURL:    "https://upload.wikimedia.org/wikipedia/en/d/d2/Money_Heist.png",
		Endings: [3]string{
			"Berlin returns from the dead and steals the Vatican",
			"Professor turns himself in to teach economics in jail",
			"Tokyo writes a tell-all and breaks the fourth wall forever",
		},
	},
	{
		ID: "mirzapur", Title: "Mirzapur", Year: 2024, Rating: 8.4,
		Genre:        "Crime",
		Synopsis:     "Power, opium and bullets shape a UP town's gangland succession.",
		ActualEnding: "Guddu seizes the gaddi after a long bloody war with Sharad.",
		PosterURL:    "https://upload.wikimedia.org/wikipedia/en/0/0e/Mirzapur_TV_series_poster.jpg",
		Endings: [3]string{
			"Kaleen Bhaiya retires to a Goa beach café peacefully",
			"Beena Tripathi takes the gaddi and exiles all the men",
			"Munna returns from the dead just to ruin Guddu's wedding",
		},
	},
	{
		ID: "panchayat", Title: "Panchayat", Year: 2024, Rating: 8.9,
		Genre:        "Comedy/Drama",
		Synopsis:     "An engineering grad becomes secretary of a Phulera village panchayat by accident.",
		ActualEnding: "Abhishek finally clears CAT but stays in Phulera for the people he loves.",
		PosterURL:    "https://upload.wikimedia.org/wikipedia/en/4/4b/Panchayat_TVF_poster.jpg",
		Endings: [3]string{
			"Pradhan ji becomes a YouTube politics influencer with 10M subs",
			"Abhishek discovers Phulera was a simulation by the UPSC",
			"Vikas opens a chai stall that goes viral on Shark Tank",
		},
	},
	{
		ID: "the-family-man", Title: "The Family Man", Year: 2021, Rating: 8.7,
		Genre:        "Spy/Thriller",
		Synopsis:     "A middle-class TASC operative juggles terror plots and his family in suburban Mumbai.",
		ActualEnding: "Srikant prevents the Chennai bio-attack but his marriage is in tatters.",
		PosterURL:    "https://upload.wikimedia.org/wikipedia/en/9/9b/The_Family_Man_TV_series_poster.jpg",
		Endings: [3]string{
			"Suchi turns out to be the actual TASC chief all along",
			"Srikant resigns and opens a budget travel agency in Pune",
			"Raji escapes and starts a NGO for retired bio-terrorists",
		},
	},
}

func (s *Server) seedAbhinavContent() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	cstmt, err := tx.Prepare(`
        INSERT INTO abhinav_content (id, kind, title, year, rating, genre, synopsis, actual_ending, poster_url, sort_order)
        VALUES (?, 'series', ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(id) DO UPDATE SET
            title=excluded.title, year=excluded.year, rating=excluded.rating,
            genre=excluded.genre, synopsis=excluded.synopsis,
            actual_ending=excluded.actual_ending, poster_url=excluded.poster_url,
            sort_order=excluded.sort_order
    `)
	if err != nil {
		return err
	}
	defer cstmt.Close()
	estmt, err := tx.Prepare(`
        INSERT INTO abhinav_endings (content_id, slot, source, author, text)
        SELECT ?, ?, 'ai', ?, ?
        WHERE NOT EXISTS (
            SELECT 1 FROM abhinav_endings WHERE content_id = ? AND slot = ? AND source = 'ai'
        )
    `)
	if err != nil {
		return err
	}
	defer estmt.Close()

	for i, sr := range abhinavSeries {
		if _, err := cstmt.Exec(sr.ID, sr.Title, sr.Year, sr.Rating, sr.Genre,
			sr.Synopsis, sr.ActualEnding, sr.PosterURL, i); err != nil {
			return err
		}
		for slot, txt := range sr.Endings {
			if _, err := estmt.Exec(sr.ID, slot+1, "AI · Series", txt, sr.ID, slot+1); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// ---------- /api/abhinav/movies ----------

type abhinavContent struct {
	ID           string         `json:"id"`
	Kind         string         `json:"kind"` // 'movie' or 'series'
	Title        string         `json:"title"`
	Year         int            `json:"year"`
	IMDBRating   float64        `json:"imdb_rating"`
	Genre        string         `json:"genre"`
	Synopsis     string         `json:"synopsis"`
	ActualEnding string         `json:"actual_ending"`
	PosterURL    string         `json:"poster_url"`
	HasAudio     bool           `json:"has_audio"`
	Endings      []abhinavEnding `json:"endings"`
}

type abhinavEnding struct {
	ID       int64   `json:"id"`
	Source   string  `json:"source"` // 'ai' | 'community'
	Author   string  `json:"author,omitempty"`
	Text     string  `json:"text"`
	Likes    int64   `json:"likes"`
	AvgRating float64 `json:"avg_rating"`
	RatingN  int64   `json:"rating_n"`
	Target   string  `json:"target"` // 'movie' (endings table) or 'series' (abhinav_endings)
}

func (s *Server) handleAbhinavMovies(w http.ResponseWriter, r *http.Request) {
	out := []abhinavContent{}

	// 1) Movies (excluding biopics) from existing tables.
	mrows, err := s.db.QueryContext(r.Context(), `
        SELECT id, title, year, imdb_rating, genre, synopsis,
               actual_ending, poster_url
        FROM movies
        ORDER BY sort_order ASC, imdb_rating DESC, year DESC
    `)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	idx := map[string]int{}
	for mrows.Next() {
		var c abhinavContent
		if err := mrows.Scan(&c.ID, &c.Title, &c.Year, &c.IMDBRating, &c.Genre,
			&c.Synopsis, &c.ActualEnding, &c.PosterURL); err != nil {
			mrows.Close()
			http.Error(w, err.Error(), 500)
			return
		}
		if abhinavBiopicExclude[c.ID] {
			continue
		}
		c.Kind = "movie"
		c.Endings = []abhinavEnding{}
		if info, err := os.Stat(filepath.Join(s.dataDir, "audio", c.ID+".mp3")); err == nil && info.Size() > 50_000 {
			c.HasAudio = true
		}
		idx[c.ID] = len(out)
		out = append(out, c)
	}
	mrows.Close()

	// Existing AI endings for those movies (target='movie' so likes go to endings table).
	erows, err := s.db.QueryContext(r.Context(), `
        SELECT id, movie_id, text, likes
        FROM endings
        ORDER BY movie_id, slot
    `)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	for erows.Next() {
		var e abhinavEnding
		var movieID string
		if err := erows.Scan(&e.ID, &movieID, &e.Text, &e.Likes); err != nil {
			erows.Close()
			http.Error(w, err.Error(), 500)
			return
		}
		e.Source = "ai"
		e.Author = "AI · Canon"
		e.Target = "movie"
		if i, ok := idx[movieID]; ok {
			out[i].Endings = append(out[i].Endings, e)
		}
	}
	erows.Close()

	// 2) Abhinav-specific content (TV series).
	srows, err := s.db.QueryContext(r.Context(), `
        SELECT id, kind, title, year, rating, genre, synopsis, actual_ending, poster_url
        FROM abhinav_content ORDER BY sort_order ASC
    `)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	sIdx := map[string]int{}
	for srows.Next() {
		var c abhinavContent
		if err := srows.Scan(&c.ID, &c.Kind, &c.Title, &c.Year, &c.IMDBRating,
			&c.Genre, &c.Synopsis, &c.ActualEnding, &c.PosterURL); err != nil {
			srows.Close()
			http.Error(w, err.Error(), 500)
			return
		}
		c.Endings = []abhinavEnding{}
		sIdx[c.ID] = len(out)
		out = append(out, c)
	}
	srows.Close()

	// Series endings (incl. community-submitted), ranked: top 3 by ai_score
	// then likes then created_at desc — Abhinav: "AI keeps top 3".
	aerows, err := s.db.QueryContext(r.Context(), `
        SELECT id, content_id, source, COALESCE(author,''),
               text, likes, rating_sum, rating_n, ai_score
        FROM abhinav_endings
        ORDER BY content_id,
                 (ai_score + likes*0.7 +
                  CASE WHEN rating_n=0 THEN 0 ELSE (rating_sum/rating_n)*0.5 END) DESC,
                 created_at DESC
    `)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	perContent := map[string]int{}
	for aerows.Next() {
		var e abhinavEnding
		var cid string
		var rsum, aiScore float64
		if err := aerows.Scan(&e.ID, &cid, &e.Source, &e.Author, &e.Text,
			&e.Likes, &rsum, &e.RatingN, &aiScore); err != nil {
			aerows.Close()
			http.Error(w, err.Error(), 500)
			return
		}
		_ = aiScore
		e.Target = "series"
		if e.RatingN > 0 {
			e.AvgRating = rsum / float64(e.RatingN)
		}
		if e.Author == "" {
			if e.Source == "community" {
				e.Author = "anonymous"
			} else {
				e.Author = "AI · Canon"
			}
		}
		if i, ok := sIdx[cid]; ok {
			// Cap at 3 displayed endings, but keep all the AI canon endings
			// even if many community endings exist.
			if perContent[cid] < 3 {
				out[i].Endings = append(out[i].Endings, e)
				perContent[cid]++
			}
		}
	}
	aerows.Close()

	body, _ := json.Marshal(map[string]any{
		"content":     out,
		"count":       len(out),
		"generated":   time.Now().UTC().Format(time.RFC3339),
		"app_version": "1.0.0+abhinav",
	})
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=10")
	_, _ = w.Write(body)
}

// ---------- /api/abhinav/like ----------

func (s *Server) handleAbhinavLike(w http.ResponseWriter, r *http.Request) {
	var body struct {
		EndingID int64  `json:"ending_id"`
		Target   string `json:"target"` // 'movie' or 'series'
		Delta    int64  `json:"delta"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	if body.EndingID == 0 || (body.Target != "movie" && body.Target != "series") {
		http.Error(w, "ending_id and target=movie|series required", 400)
		return
	}
	if body.Delta == 0 {
		body.Delta = 1
	}
	if body.Delta > 1 {
		body.Delta = 1
	}
	if body.Delta < -1 {
		body.Delta = -1
	}
	tbl := "endings"
	if body.Target == "series" {
		tbl = "abhinav_endings"
	}
	if _, err := s.db.ExecContext(r.Context(),
		fmt.Sprintf(`UPDATE %s SET likes = MAX(0, likes + ?) WHERE id = ?`, tbl),
		body.Delta, body.EndingID); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	var likes int64
	_ = s.db.QueryRowContext(r.Context(),
		fmt.Sprintf(`SELECT likes FROM %s WHERE id = ?`, tbl), body.EndingID).Scan(&likes)
	s.bustCache()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "likes": likes})
}

// ---------- /api/abhinav/submit-ending ----------

func (s *Server) handleAbhinavSubmitEnding(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8*1024)
	var body struct {
		ContentID string `json:"content_id"`
		Text      string `json:"text"`
		Author    string `json:"author"`
		AnonID    string `json:"anon_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	body.Text = strings.TrimSpace(body.Text)
	body.Author = strings.TrimSpace(body.Author)
	if body.ContentID == "" || body.Text == "" {
		http.Error(w, "content_id + text required", 400)
		return
	}
	if len([]rune(body.Text)) > 200 {
		body.Text = string([]rune(body.Text)[:200])
	}
	if len(body.Author) == 0 {
		body.Author = "anonymous"
	}
	if len(body.Author) > 32 {
		body.Author = body.Author[:32]
	}

	// content_id must exist in either abhinav_content or movies-without-biopic.
	var ok bool
	_ = s.db.QueryRowContext(r.Context(),
		`SELECT EXISTS(SELECT 1 FROM abhinav_content WHERE id = ?)`, body.ContentID).Scan(&ok)
	if !ok {
		var inMovies bool
		_ = s.db.QueryRowContext(r.Context(),
			`SELECT EXISTS(SELECT 1 FROM movies WHERE id = ?)`, body.ContentID).Scan(&inMovies)
		if !inMovies || abhinavBiopicExclude[body.ContentID] {
			http.Error(w, "unknown content_id", 404)
			return
		}
	}

	// AI heuristic score: 0.4 * (text-length factor) + 0.6 * (uniqueness vs canon).
	// Cheap and good enough for a beta.
	aiScore := scoreEnding(body.Text)

	// Find next slot for this content_id.
	var nextSlot int
	_ = s.db.QueryRowContext(r.Context(),
		`SELECT COALESCE(MAX(slot),0)+1 FROM abhinav_endings WHERE content_id = ?`,
		body.ContentID).Scan(&nextSlot)
	if nextSlot < 4 {
		nextSlot = 4 // canon endings (1..3) reserved for AI
	}

	res, err := s.db.ExecContext(r.Context(), `
        INSERT INTO abhinav_endings (content_id, slot, source, author, text, ai_score)
        VALUES (?, ?, 'community', ?, ?, ?)
    `, body.ContentID, nextSlot, body.Author, body.Text, aiScore)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	id, _ := res.LastInsertId()
	s.bustCache()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "ending_id": id, "ai_score": aiScore,
	})
}

// scoreEnding is a tiny heuristic scoring function. We're not running a model
// per submission — that's overkill for a beta — but the column lets us
// upgrade later without changing schema.
func scoreEnding(text string) float64 {
	t := strings.TrimSpace(text)
	n := len([]rune(t))
	// Sweet spot is 30-90 runes — Instagram-caption length.
	lenScore := 0.0
	switch {
	case n < 8:
		lenScore = 0.1
	case n < 30:
		lenScore = 0.6
	case n <= 90:
		lenScore = 1.0
	case n <= 140:
		lenScore = 0.8
	default:
		lenScore = 0.5
	}
	// Punctuation = expressiveness (rough proxy).
	punct := 0
	for _, r := range t {
		switch r {
		case '!', '?', ',', '—', '.':
			punct++
		}
	}
	puncScore := float64(min(punct, 5)) / 5.0
	return 0.7*lenScore + 0.3*puncScore
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------- /api/abhinav/rate-ending ----------

func (s *Server) handleAbhinavRateEnding(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4*1024)
	var body struct {
		EndingID int64  `json:"ending_id"`
		Target   string `json:"target"`
		Rating   int    `json:"rating"`
		AnonID   string `json:"anon_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	if body.EndingID == 0 || body.AnonID == "" {
		http.Error(w, "ending_id+anon_id required", 400)
		return
	}
	if body.Target != "movie" && body.Target != "series" {
		body.Target = "series"
	}
	if body.Rating < 1 {
		body.Rating = 1
	}
	if body.Rating > 5 {
		body.Rating = 5
	}

	tx, err := s.db.Begin()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer tx.Rollback()

	// Upsert the rating; on update we adjust the rating_sum delta on the parent.
	var prev int
	row := tx.QueryRowContext(r.Context(),
		`SELECT rating FROM abhinav_ratings WHERE ending_id = ? AND target = ? AND anon_id = ?`,
		body.EndingID, body.Target, body.AnonID)
	previousFound := row.Scan(&prev) == nil

	if previousFound {
		_, _ = tx.ExecContext(r.Context(),
			`UPDATE abhinav_ratings SET rating = ?, created_at = ?
              WHERE ending_id = ? AND target = ? AND anon_id = ?`,
			body.Rating, time.Now().UnixMilli(), body.EndingID, body.Target, body.AnonID)
	} else {
		_, _ = tx.ExecContext(r.Context(),
			`INSERT INTO abhinav_ratings (ending_id, target, anon_id, rating, created_at)
              VALUES (?, ?, ?, ?, ?)`,
			body.EndingID, body.Target, body.AnonID, body.Rating, time.Now().UnixMilli())
	}

	// Apply delta to abhinav_endings.rating_sum / rating_n.
	if body.Target == "series" {
		if previousFound {
			_, _ = tx.ExecContext(r.Context(),
				`UPDATE abhinav_endings SET rating_sum = rating_sum + ?
                  WHERE id = ?`, float64(body.Rating-prev), body.EndingID)
		} else {
			_, _ = tx.ExecContext(r.Context(),
				`UPDATE abhinav_endings SET rating_sum = rating_sum + ?, rating_n = rating_n + 1
                  WHERE id = ?`, float64(body.Rating), body.EndingID)
		}
	}

	if err := tx.Commit(); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	var avg float64
	var n int64
	_ = s.db.QueryRowContext(r.Context(),
		`SELECT CASE WHEN rating_n=0 THEN 0 ELSE rating_sum/rating_n END, rating_n
          FROM abhinav_endings WHERE id = ?`, body.EndingID).Scan(&avg, &n)
	s.bustCache()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "avg_rating": avg, "rating_n": n, "your_rating": body.Rating,
	})
}

// ---------- frontend serving ----------

// handleAbhinavFrontend serves /abhinav and /abhinav/* from the
// frontend-abhinav directory. We don't use http.FileServer because it
// helpfully redirects /foo/index.html to /foo/, which would bounce back
// out of the /abhinav prefix and end up on the main static handler.
func (s *Server) handleAbhinavFrontend() http.Handler {
	dir := filepath.Join(filepath.Dir(s.frontendDir), "frontend-abhinav")
	if _, err := os.Stat(dir); err != nil {
		// Sibling dir missing — fall back to the regular frontend folder so
		// devs running outside the repo root don't see a 500.
		dir = s.frontendDir
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/abhinav")
		if p == "" || p == "/" {
			p = "/index.html"
		}
		// Sanitize: no traversal, no backslash.
		clean := filepath.FromSlash(strings.TrimPrefix(p, "/"))
		if strings.Contains(clean, "..") {
			http.NotFound(w, r)
			return
		}
		full := filepath.Join(dir, clean)
		info, err := os.Stat(full)
		if err != nil || info.IsDir() {
			// SPA fallback — serve index.html for unknown routes.
			full = filepath.Join(dir, "index.html")
			info, err = os.Stat(full)
			if err != nil {
				http.NotFound(w, r)
				return
			}
		}
		// Cache headers.
		switch {
		case strings.HasSuffix(p, ".html"), p == "/index.html":
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		case strings.HasSuffix(p, ".js"):
			w.Header().Set("Cache-Control", "public, max-age=300")
			w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		case strings.HasSuffix(p, ".css"):
			w.Header().Set("Cache-Control", "public, max-age=300")
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		case strings.HasSuffix(p, ".webmanifest"):
			w.Header().Set("Content-Type", "application/manifest+json")
		default:
			w.Header().Set("Cache-Control", "public, max-age=86400")
		}
		// Serve the file ourselves — http.ServeFile redirects /index.html
		// to /, which would bounce out of the /abhinav prefix.
		f, err := os.Open(full)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		http.ServeContent(w, r, info.Name(), info.ModTime(), f)
	})
}

// abhinavCtx is unused for now — placeholder so future helpers can pass a
// validation context if needed.
var abhinavCtx = context.Background()

func init() {
	_ = errors.New // anchor imports
}
