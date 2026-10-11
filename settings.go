package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// Settings are edited on the settings page and stored in the database.
// Environment variables only provide the first defaults.
type Settings struct {
	Announce        AnnounceSettings   `json:"announce"`
	TextSearch      TextSearchSettings `json:"text_search"`
	Limits          map[string]string  `json:"limits"` // per API, e.g. "1/2s,300/1h"
	WhisperLanguage string             `json:"whisper_language"`

	// Path rules: ordered regexes over the path of part 1; the first match wins.
	PathRules      []PathRule `json:"path_rules"`
	PathAccept     bool       `json:"path_accept"`      // treat a path match with author and title as matched, skipping listening
	PathCrossCheck bool       `json:"path_cross_check"` // send audio matches that disagree with the path to review
}

// AnnounceSettings configure method 1: listen to the opening for
// "[Someone presents:] Title, by Author[, read by Narrator]".
type AnnounceSettings struct {
	Enabled   bool     `json:"enabled"`
	Start     int      `json:"start"`   // seconds into part 1
	Seconds   int      `json:"seconds"` // sample length
	UseLLM    bool     `json:"use_llm"` // ask the LLM when the pattern finds no title and author
	Catalogs  []string `json:"catalogs"`
	MatchPct  int      `json:"match_pct"`  // score that counts as a match
	ReviewPct int      `json:"review_pct"` // score worth showing for review
}

// TextSearchSettings configure method 2: transcribe a random stretch from
// the middle of the book and search catalogs that index book text.
type TextSearchSettings struct {
	Enabled      bool     `json:"enabled"`
	Seconds      int      `json:"seconds"`       // length of each sample
	MiddlePct    int      `json:"middle_pct"`    // sample only from the middle N% of the set
	ExtraSamples int      `json:"extra_samples"` // more samples to add while 2+ books match
	MaxWords     int      `json:"max_words"`     // cap on words sent per search
	Catalogs     []string `json:"catalogs"`
	AcceptSingle bool     `json:"accept_single"` // one book found = matched (otherwise review)
}

// Which catalogs each method can use.
var methodCatalogs = map[string][]string{
	"announce":    {"audible", "openlibrary", "googlebooks"},
	"text_search": {"googlebooks", "openlibrary"},
}

func defaultSettings(cfg Config) Settings {
	limits := map[string]string{}
	parsed, _ := parseLimits(cfg.Limits)
	for name, ws := range parsed {
		limits[name] = formatLimits(ws)
	}
	return Settings{
		Announce: AnnounceSettings{Enabled: true, Start: cfg.SampleStart, Seconds: 15, UseLLM: cfg.OllamaURL != "",
			Catalogs: []string{"audible", "openlibrary", "googlebooks"}, MatchPct: 80, ReviewPct: 45},
		TextSearch: TextSearchSettings{Enabled: true, Seconds: 30, MiddlePct: 80, ExtraSamples: 1, MaxWords: 150,
			Catalogs: []string{"googlebooks", "openlibrary"}, AcceptSingle: true},
		Limits:          limits,
		WhisperLanguage: cfg.WhisperLang,
		PathRules:       defaultPathRules,
		PathCrossCheck:  true,
	}
}

func formatLimits(ws []Window) string {
	var parts []string
	for _, w := range ws {
		parts = append(parts, fmt.Sprintf("%d/%s", w.N, shortDur(w.Per)))
	}
	return strings.Join(parts, ",")
}

func shortDur(d interface{ String() string }) string {
	s := d.String()
	s = strings.TrimSuffix(s, "0s")
	s = strings.TrimSuffix(s, "0m")
	if s == "" {
		return "0s"
	}
	return s
}

func loadSettings(db *sql.DB, cfg Config) Settings {
	s := defaultSettings(cfg)
	var raw string
	if db.QueryRow(`SELECT value FROM settings WHERE key='settings'`).Scan(&raw) == nil {
		json.Unmarshal([]byte(raw), &s) // stored values override defaults; new fields keep defaults
		if reflect.DeepEqual(s.PathRules, oldDefaultPathRules) {
			s.PathRules = defaultPathRules
		}
	}
	return s
}

func saveSettings(db *sql.DB, s Settings) error {
	_, err := db.Exec(`INSERT INTO settings(key, value) VALUES('settings', ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, mustJSON(s))
	return err
}

func (s Settings) validate() error {
	a, t := s.Announce, s.TextSearch
	switch {
	case a.Seconds < 5 || a.Seconds > 120:
		return fmt.Errorf("announcement sample must be 5 to 120 seconds")
	case a.Start < 0:
		return fmt.Errorf("announcement start can't be negative")
	case a.ReviewPct < 0 || a.MatchPct > 100 || a.ReviewPct > a.MatchPct:
		return fmt.Errorf("review score must be between 0 and the match score, and match at most 100")
	case t.Seconds < 5 || t.Seconds > 120:
		return fmt.Errorf("text search sample must be 5 to 120 seconds")
	case t.MiddlePct < 10 || t.MiddlePct > 100:
		return fmt.Errorf("middle portion must be 10 to 100 percent")
	case t.ExtraSamples < 0 || t.ExtraSamples > 5:
		return fmt.Errorf("extra samples must be 0 to 5")
	case t.MaxWords < 10:
		return fmt.Errorf("max words must be at least 10")
	}
	if _, err := compilePathRules(s.PathRules); err != nil {
		return err
	}
	for name, spec := range s.Limits {
		if _, err := parseLimits(name + "=" + spec); err != nil {
			return err
		}
	}
	return nil
}

func contains1(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
