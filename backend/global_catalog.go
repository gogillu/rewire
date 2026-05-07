// global_catalog.go — v1.5.0: schema migrations and seed-side helpers for
// the "global catalog" expansion (Hollywood movies + TV series alongside
// the existing 100 Bollywood titles).
//
// Two new movie columns:
//   region  — 'bollywood' (default), 'hollywood', 'world'
//   kind    — 'movie' (default), 'tv'
//
// Both default-fill to the existing values so older rows continue to work
// unchanged. Premium tier filters by user prefs.categories[]; the free /
// /sagar / /abhinav frontends keep showing only bollywood movies.
package main

import "strings"

// migrateGlobalColumns idempotently adds region+kind to movies. Old rows
// keep showing as bollywood/movie which preserves all current behaviour.
func (s *Server) migrateGlobalColumns() {
	stmts := []string{
		`ALTER TABLE movies ADD COLUMN region TEXT NOT NULL DEFAULT 'bollywood'`,
		`ALTER TABLE movies ADD COLUMN kind   TEXT NOT NULL DEFAULT 'movie'`,
	}
	for _, q := range stmts {
		_, err := s.db.Exec(q)
		if err != nil && !strings.Contains(err.Error(), "duplicate column") {
			_ = err
		}
	}
	// Ensure existing rows with NULL region/kind (sqlite quirk on older
	// schemas where NOT NULL DEFAULT was relaxed) get sane values.
	_, _ = s.db.Exec(`UPDATE movies SET region = 'bollywood' WHERE region IS NULL OR region = ''`)
	_, _ = s.db.Exec(`UPDATE movies SET kind   = 'movie'     WHERE kind   IS NULL OR kind   = ''`)
}

// regionKindAllowed is the canonical truth-set used by the premium filter
// and by the seed JSON validator.
func regionKindAllowed(region, kind string) (string, string) {
	switch strings.ToLower(strings.TrimSpace(region)) {
	case "hollywood":
		region = "hollywood"
	case "world", "global", "international":
		region = "world"
	default:
		region = "bollywood"
	}
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "tv", "series", "tv-series", "show":
		kind = "tv"
	default:
		kind = "movie"
	}
	return region, kind
}

// filterMoviesByCategories applies the user's category preference list to
// the full catalog. Pref tokens recognised:
//   bollywood   → bollywood movies
//   hollywood   → hollywood movies
//   tv-in       → bollywood (Indian) tv series
//   tv-foreign  → hollywood/world tv series
// Empty prefs default to bollywood (caller's responsibility).
func filterMoviesByCategories(movies []Movie, cats []string) []Movie {
	if len(cats) == 0 {
		return movies
	}
	allowBollyMovie := false
	allowHollyMovie := false
	allowTvIn := false
	allowTvForeign := false
	for _, c := range cats {
		switch strings.ToLower(strings.TrimSpace(c)) {
		case "bollywood":
			allowBollyMovie = true
		case "hollywood":
			allowHollyMovie = true
		case "tv-in":
			allowTvIn = true
		case "tv-foreign":
			allowTvForeign = true
		}
	}
	out := make([]Movie, 0, len(movies))
	for _, m := range movies {
		region, kind := regionKindAllowed(m.Region, m.Kind)
		switch {
		case kind == "tv" && region == "bollywood" && allowTvIn:
			out = append(out, m)
		case kind == "tv" && region != "bollywood" && allowTvForeign:
			out = append(out, m)
		case kind == "movie" && region == "bollywood" && allowBollyMovie:
			out = append(out, m)
		case kind == "movie" && region == "hollywood" && allowHollyMovie:
			out = append(out, m)
		case kind == "movie" && region == "world" && allowHollyMovie:
			out = append(out, m)
		}
	}
	return out
}
