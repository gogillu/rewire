// Package main — geo helpers shared with telemetry.
//
// The base resolver lives in telemetry.go; this file just adds:
//   1. migrateRichGeo — adds the new ip_geo columns idempotently.
//   2. geoLookupRich — returns (country, city, isp) for richer flag/feedback
//      logging without changing the existing (country, city) callers.
package main

import "strings"

func (s *Server) migrateRichGeo() {
	stmts := []string{
		`ALTER TABLE ip_geo ADD COLUMN district TEXT`,
		`ALTER TABLE ip_geo ADD COLUMN zip TEXT`,
		`ALTER TABLE ip_geo ADD COLUMN lat REAL`,
		`ALTER TABLE ip_geo ADD COLUMN lon REAL`,
		`ALTER TABLE ip_geo ADD COLUMN asn TEXT`,
		`ALTER TABLE ip_geo ADD COLUMN org TEXT`,
		`ALTER TABLE ip_geo ADD COLUMN is_mobile INTEGER DEFAULT 0`,
		`ALTER TABLE ip_geo ADD COLUMN is_proxy INTEGER DEFAULT 0`,
		`ALTER TABLE ip_geo ADD COLUMN is_hosting INTEGER DEFAULT 0`,
	}
	for _, q := range stmts {
		_, err := s.db.Exec(q)
		if err != nil && !strings.Contains(err.Error(), "duplicate column") {
			_ = err
		}
	}
}

func (s *Server) geoLookupRich(ip string) (country, city, isp string) {
	if s.geo == nil || ip == "" {
		return
	}
	s.geo.mu.RLock()
	defer s.geo.mu.RUnlock()
	if e, ok := s.geo.mem[ip]; ok {
		return e.country, e.city, e.isp
	}
	return
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
