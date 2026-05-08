// poster_overrides.go — v1.5.1: hardcoded poster URLs for titles that
// Wikipedia REST keeps misrouting (apostrophes, ASCII em-dashes,
// disambig-page winners, etc.). Checked BEFORE Wikipedia is queried,
// so these are authoritative.
//
// All URLs are direct upload.wikimedia.org thumbnails copied from the
// matching Wikipedia article infobox. Stable, no API call, no
// rate-limit risk.
package main

var posterOverrides = map[string]string{
	// ---------- Bollywood movies (14) ----------
	"andaz-apna-apna":   "https://upload.wikimedia.org/wikipedia/en/c/c9/Andaz_Apna_Apna_-_Poster.jpg",
	"bajrangi-bhaijaan": "https://upload.wikimedia.org/wikipedia/en/2/26/Bajrangi_Bhaijaan_Poster.jpg",
	"dear-zindagi":      "https://upload.wikimedia.org/wikipedia/en/4/40/Dear_Zindagi_poster.jpg",
	"gow-2":             "https://upload.wikimedia.org/wikipedia/en/4/4e/Gangs_of_Wasseypur_-_Part_2.jpg",
	"jersey-2022":       "https://upload.wikimedia.org/wikipedia/en/d/db/Jersey_%282022_film%29_poster.jpg",
	"kabhi-khushi":      "https://upload.wikimedia.org/wikipedia/en/2/29/Kabhi_Khushi_Kabhie_Gham_Poster.jpg",
	"khosla-ka-ghosla":  "https://upload.wikimedia.org/wikipedia/en/8/8b/Khosla_Ka_Ghosla.jpg",
	"krish-3":           "https://upload.wikimedia.org/wikipedia/en/8/89/Krrish_3_poster.jpg",
	"masaan":            "https://upload.wikimedia.org/wikipedia/en/4/4e/Masaan_film_poster.jpg",
	"mulk":              "https://upload.wikimedia.org/wikipedia/en/8/86/Mulk_poster.jpg",
	"my-name-is-khan":   "https://upload.wikimedia.org/wikipedia/en/9/96/My_Name_Is_Khan_poster.jpg",
	"omkara":            "https://upload.wikimedia.org/wikipedia/en/8/86/Omkara2006film.jpg",
	"padmaavat":         "https://upload.wikimedia.org/wikipedia/en/f/fb/Padmaavat_poster.jpg",
	"thappad":           "https://upload.wikimedia.org/wikipedia/en/d/db/Thappad_poster.jpg",

	// ---------- Bollywood TV / web (9) ----------
	"aspirants":     "https://upload.wikimedia.org/wikipedia/en/9/9b/Aspirants_TVF_logo.jpg",
	"delhi-crime":   "https://upload.wikimedia.org/wikipedia/en/0/06/Delhi_Crime.jpg",
	"gullak":        "https://upload.wikimedia.org/wikipedia/en/8/82/Gullak_TVF_logo.jpg",
	"kota-factory":  "https://upload.wikimedia.org/wikipedia/en/c/cd/Kota_Factory_TVF_logo.jpg",
	"paatal-lok":    "https://upload.wikimedia.org/wikipedia/en/3/30/Paatal_Lok_poster.jpg",
	"panchayat":     "https://upload.wikimedia.org/wikipedia/en/7/72/Panchayat_TVF_logo.jpg",
	"sacred-games":  "https://upload.wikimedia.org/wikipedia/en/3/3d/Sacred_Games_logo.png",
	"scam-1992":     "https://upload.wikimedia.org/wikipedia/en/2/2e/Scam_1992_The_Harshad_Mehta_Story.jpg",
	"the-family-man": "https://upload.wikimedia.org/wikipedia/en/4/41/The_Family_Man_TV_series.jpg",

	// ---------- Hollywood movies (19) ----------
	"avengers-endgame":         "https://upload.wikimedia.org/wikipedia/en/0/0d/Avengers_Endgame_poster.jpg",
	"fight-club":               "https://upload.wikimedia.org/wikipedia/en/f/fc/Fight_Club_poster.jpg",
	"forrest-gump":             "https://upload.wikimedia.org/wikipedia/en/6/67/Forrest_Gump_poster.jpg",
	"gladiator":                "https://upload.wikimedia.org/wikipedia/en/8/8d/Gladiator_%282000_film_poster%29.png",
	"gone-girl":                "https://upload.wikimedia.org/wikipedia/en/0/05/Gone_Girl_Poster.jpg",
	"goodfellas":               "https://upload.wikimedia.org/wikipedia/en/7/7b/Goodfellas.jpg",
	"la-la-land":               "https://upload.wikimedia.org/wikipedia/en/a/ab/La_La_Land_%28film%29.png",
	"no-country-for-old-men":   "https://upload.wikimedia.org/wikipedia/en/8/8b/No_Country_for_Old_Men_poster.jpg",
	"pulp-fiction":             "https://upload.wikimedia.org/wikipedia/en/3/3b/Pulp_Fiction_%281994%29_poster.jpg",
	"saving-private-ryan":      "https://upload.wikimedia.org/wikipedia/en/a/ac/Saving_Private_Ryan_poster.jpg",
	"schindlers-list":          "https://upload.wikimedia.org/wikipedia/en/3/38/Schindler%27s_List_movie.jpg",
	"the-dark-knight":          "https://upload.wikimedia.org/wikipedia/en/8/8a/Dark_Knight.jpg",
	"the-godfather":            "https://upload.wikimedia.org/wikipedia/en/1/1c/Godfather_ver1.jpg",
	"the-green-mile":           "https://upload.wikimedia.org/wikipedia/en/d/de/Green_mile_ver2.jpg",
	"the-lion-king":            "https://upload.wikimedia.org/wikipedia/en/3/3d/The_Lion_King_poster.jpg",
	"the-prestige":             "https://upload.wikimedia.org/wikipedia/en/d/d2/Prestige_poster.jpg",
	"the-shawshank-redemption": "https://upload.wikimedia.org/wikipedia/en/8/81/ShawshankRedemptionMoviePoster.jpg",
	"titanic":                  "https://upload.wikimedia.org/wikipedia/en/1/19/Titanic_%28Official_Film_Poster%29.png",
	"whiplash":                 "https://upload.wikimedia.org/wikipedia/en/0/01/Whiplash_poster.jpg",

	// ---------- Hollywood / British TV (7) ----------
	"breaking-bad":     "https://upload.wikimedia.org/wikipedia/en/6/61/Breaking_Bad_title_card.png",
	"friends":          "https://upload.wikimedia.org/wikipedia/en/4/41/Friends_logo.svg",
	"game-of-thrones":  "https://upload.wikimedia.org/wikipedia/en/1/1d/Game_of_Thrones_2011_logo.svg",
	"house-of-cards":   "https://upload.wikimedia.org/wikipedia/en/8/8a/House_of_Cards_title_card.jpg",
	"peaky-blinders":   "https://upload.wikimedia.org/wikipedia/en/9/9f/Peaky_Blinders_title_card.jpg",
	"stranger-things":  "https://upload.wikimedia.org/wikipedia/en/3/38/Stranger_Things_logo.png",
	"the-office-us":    "https://upload.wikimedia.org/wikipedia/en/9/95/The_Office_US_logo.png",

	// ---------- World (4) ----------
	"parasite":    "https://upload.wikimedia.org/wikipedia/en/5/53/Parasite_%282019_film%29.png",
	"dark":        "https://upload.wikimedia.org/wikipedia/en/4/4c/Dark_%28TV_series%29.jpg",
	"money-heist": "https://upload.wikimedia.org/wikipedia/en/7/7c/Money_Heist_logo.png",
	"squid-game":  "https://upload.wikimedia.org/wikipedia/en/1/1c/Squid_Game_logo.png",
}
