// posters.go — backfill missing poster URLs from Wikipedia at startup.
//
// Wikipedia's REST summary endpoint is open, fast, and CORS-friendly:
//
//   GET https://en.wikipedia.org/api/rest_v1/page/summary/<title>
//
// The "thumbnail.source" / "originalimage.source" fields point to a
// canonical poster image for film articles. We try a small list of title
// variants ("Foo (film)", "Foo (YYYY film)", "Foo") and stop at the first
// hit. Results persist to the DB so we only call Wikipedia once per movie.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type wikiSummary struct {
	Type        string `json:"type"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Thumbnail   struct {
		Source string `json:"source"`
		Width  int    `json:"width"`
		Height int    `json:"height"`
	} `json:"thumbnail"`
	OriginalImage struct {
		Source string `json:"source"`
	} `json:"originalimage"`
}

func (s *Server) backfillPosters(ctx context.Context) {
	// v1.5.1 — clear stale "none" sentinels so the new region/kind-aware
	// resolver gets another shot at the 50+ Hollywood/TV stragglers that
	// the v1.5.0 Bollywood-only variant list could not find.
	_, _ = s.db.ExecContext(ctx, `UPDATE movies SET poster_url = '' WHERE poster_url = 'none'`)

	rows, err := s.db.QueryContext(ctx, `
        SELECT id, title, year, COALESCE(region,'bollywood'), COALESCE(kind,'movie')
        FROM movies
        WHERE poster_url IS NULL OR poster_url = ''
    `)
	if err != nil {
		log.Printf("rewire: poster backfill query: %v", err)
		return
	}
	type pending struct {
		ID, Title, Region, Kind string
		Year                    int
	}
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.ID, &p.Title, &p.Year, &p.Region, &p.Kind); err == nil {
			todo = append(todo, p)
		}
	}
	rows.Close()
	if len(todo) == 0 {
		return
	}
	log.Printf("rewire: poster backfill: %d movie(s) need a poster", len(todo))

	client := &http.Client{Timeout: 10 * time.Second}
	for _, p := range todo {
		select {
		case <-ctx.Done():
			return
		default:
		}
		url := ""
		if hard, ok := posterOverrides[p.ID]; ok && hard != "" {
			url = hard
		} else {
			url, _ = fetchWikiPoster(client, p.Title, p.Year, p.Region, p.Kind)
		}
		if url == "" {
			// Mark as attempted so we don't retry on every boot. Use a
			// known sentinel — empty string already triggers retry, so
			// store the literal "none" instead.
			_, _ = s.db.ExecContext(ctx,
				`UPDATE movies SET poster_url = 'none' WHERE id = ? AND (poster_url IS NULL OR poster_url = '')`, p.ID)
			continue
		}
		if _, err := s.db.ExecContext(ctx,
			`UPDATE movies SET poster_url = ? WHERE id = ?`, url, p.ID); err != nil {
			log.Printf("rewire: poster save %s: %v", p.ID, err)
		}
		// Be polite to Wikipedia.
		time.Sleep(150 * time.Millisecond)
	}
	// Promote sentinel "none" back to empty for the API layer so the frontend
	// renders its own gradient placeholder.
	_, _ = s.db.ExecContext(ctx, `UPDATE movies SET poster_url = '' WHERE poster_url = 'none'`)
	s.bustCache()
	log.Printf("rewire: poster backfill done")
}

func fetchWikiPoster(client *http.Client, title string, year int, region, kind string) (string, error) {
	// Title variants — region+kind aware. Wikipedia disambig parents keep
	// stray pages like "Schindler's List" pointing at the article (no suffix
	// needed) but TV and Hollywood movies more often need a qualifier.
	variants := []string{}
	if kind == "tv" {
		if year > 0 {
			variants = append(variants, fmt.Sprintf("%s (%d TV series)", title, year))
		}
		switch region {
		case "bollywood":
			variants = append(variants,
				fmt.Sprintf("%s (Indian TV series)", title),
				fmt.Sprintf("%s (Hindi TV series)", title),
				fmt.Sprintf("%s (web series)", title),
				fmt.Sprintf("%s (Indian web series)", title),
			)
		case "hollywood":
			variants = append(variants,
				fmt.Sprintf("%s (American TV series)", title),
				fmt.Sprintf("%s (British TV series)", title),
			)
		case "world":
			variants = append(variants,
				fmt.Sprintf("%s (South Korean TV series)", title),
				fmt.Sprintf("%s (Spanish TV series)", title),
				fmt.Sprintf("%s (German TV series)", title),
			)
		}
		variants = append(variants,
			fmt.Sprintf("%s (TV series)", title),
			title,
		)
	} else {
		if year > 0 {
			variants = append(variants, fmt.Sprintf("%s (%d film)", title, year))
		}
		switch region {
		case "bollywood":
			variants = append(variants,
				fmt.Sprintf("%s (Hindi film)", title),
				fmt.Sprintf("%s (Indian film)", title),
			)
		case "hollywood":
			variants = append(variants,
				fmt.Sprintf("%s (American film)", title),
				fmt.Sprintf("%s (British film)", title),
			)
		case "world":
			variants = append(variants,
				fmt.Sprintf("%s (South Korean film)", title),
				fmt.Sprintf("%s (Spanish film)", title),
			)
		}
		variants = append(variants,
			fmt.Sprintf("%s (film)", title),
			title,
		)
	}
	for _, v := range variants {
		path := url.PathEscape(strings.ReplaceAll(v, " ", "_"))
		api := "https://en.wikipedia.org/api/rest_v1/page/summary/" + path
		req, _ := http.NewRequest("GET", api, nil)
		req.Header.Set("User-Agent", "rewire/0.1 (+gogillu.live; arushi)")
		req.Header.Set("Accept", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			continue
		}
		var w wikiSummary
		if err := json.Unmarshal(body, &w); err != nil {
			continue
		}
		// Skip disambiguation pages.
		if strings.Contains(strings.ToLower(w.Description), "disambiguation") {
			continue
		}
		if w.OriginalImage.Source != "" {
			return w.OriginalImage.Source, nil
		}
		if w.Thumbnail.Source != "" {
			return w.Thumbnail.Source, nil
		}
	}
	return "", nil
}
