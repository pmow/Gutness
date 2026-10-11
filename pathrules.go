package main

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// A PathRule reads book details out of a file path with a regular
// expression. Rules are tried in order and the first that matches wins.
// Named groups supply the details: author, title, series, index, narrator, year.
type PathRule struct {
	Name    string `json:"name"`
	Pattern string `json:"pattern"`
	Enabled bool   `json:"enabled"`
}

type PathMatch struct {
	Rule        string `json:"rule,omitempty"`
	Subject     string `json:"subject,omitempty"`
	Author      string `json:"author,omitempty"`
	Title       string `json:"title,omitempty"`
	Series      string `json:"series,omitempty"`
	SeriesIndex string `json:"index,omitempty"`
	Narrator    string `json:"narrator,omitempty"`
	Year        string `json:"year,omitempty"`
}

func (m PathMatch) Found() bool { return m.Rule != "" }

var pathGroups = []string{"author", "title", "series", "index", "narrator", "year"}

// Rules see the set's folder path inside its library, e.g. for the library
// /audiobooks, the set /audiobooks/a/b/c/d is matched as "a/b/c/d".
var defaultPathRules = []PathRule{
	{Name: "Author/Series/# - Title", Enabled: true,
		Pattern: `^(?:.*/)?(?P<author>[^/]+)/(?P<series>[^/]+)/(?:Book |Vol\.? ?|#)?(?P<index>\d{1,3}(?:\.\d+)?)\s*[-.]\s*(?P<title>[^/]+)$`},
	{Name: "Series/# - Title", Enabled: true,
		Pattern: `^(?P<series>[^/]+)/(?:Book |Vol\.? ?|#)?(?P<index>\d{1,3}(?:\.\d+)?)\s*[-.]\s*(?P<title>[^/]+)$`},
	{Name: "Author - Title folder", Enabled: true,
		Pattern: `^(?:.*/)?(?P<author>[^/]+?) - (?P<title>[^/]+?)(?: \((?P<year>\d{4})\))?$`},
	{Name: "Author/Title", Enabled: true,
		Pattern: `^(?P<author>[^/]+)/(?P<title>[^/]+)$`},
}

// oldDefaultPathRules matched the path of the first file; settings
// still holding them untouched are moved to the new defaults.
var oldDefaultPathRules = []PathRule{
	{Name: "Author/Series/# - Title", Enabled: true,
		Pattern: `^(?P<author>[^/]+)/(?P<series>[^/]+)/(?:Book |Vol\.? ?|#)?(?P<index>\d{1,3}(?:\.\d+)?)\s*[-.]\s*(?P<title>[^/]+)/[^/]+$`},
	{Name: "Author - Title folder", Enabled: true,
		Pattern: `^(?:.*/)?(?P<author>[^/]+?) - (?P<title>[^/]+?)(?: \((?P<year>\d{4})\))?/[^/]+$`},
	{Name: "Author/Title", Enabled: true,
		Pattern: `^(?P<author>[^/]+)/(?P<title>[^/]+)/[^/]+$`},
}

type compiledRule struct {
	PathRule
	re *regexp.Regexp
}

// compilePathRules checks every rule; each needs a valid pattern with a title group.
func compilePathRules(rules []PathRule) ([]compiledRule, error) {
	var out []compiledRule
	for i, r := range rules {
		if strings.TrimSpace(r.Pattern) == "" {
			continue
		}
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			return nil, fmt.Errorf("path rule %d (%s): %v", i+1, r.Name, err)
		}
		if re.SubexpIndex("title") < 0 {
			return nil, fmt.Errorf("path rule %d (%s) needs a (?P<title>…) group", i+1, r.Name)
		}
		out = append(out, compiledRule{r, re})
	}
	return out, nil
}

// pathSubject is what the rules are matched against: the set's folder
// inside its library, with forward slashes.
func pathSubject(root string, it *Item) string {
	p := it.Folder
	if rel, err := filepath.Rel(root, p); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
		p = rel
	}
	return filepath.ToSlash(p)
}

func matchPath(rules []compiledRule, subject string) PathMatch {
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		m := r.re.FindStringSubmatch(subject)
		if m == nil {
			continue
		}
		get := func(g string) string {
			if i := r.re.SubexpIndex(g); i >= 0 && i < len(m) {
				return strings.Join(strings.Fields(strings.ReplaceAll(m[i], "_", " ")), " ")
			}
			return ""
		}
		pm := PathMatch{Rule: r.Name, Subject: subject, Author: get("author"), Title: get("title"), Series: get("series"),
			SeriesIndex: get("index"), Narrator: get("narrator"), Year: get("year")}
		if pm.Title == "" {
			continue // a rule only counts when it yields a title
		}
		return pm
	}
	return PathMatch{Subject: subject}
}

// applyPathRules recomputes the path match of every item (or one library).
func applyPathRules(db *sql.DB, s Settings, libraryID int64) error {
	rules, err := compilePathRules(s.PathRules)
	if err != nil {
		return err
	}
	roots := map[int64]string{}
	for _, l := range listLibraries(db) {
		roots[l.ID] = l.Path
	}
	q, args := `SELECT `+itemCols+` FROM items`, []any{}
	if libraryID != 0 {
		q += ` WHERE library_id=?`
		args = append(args, libraryID)
	}
	rows, err := db.Query(q, args...)
	if err != nil {
		return err
	}
	var items []*Item
	for rows.Next() {
		if it, err := scanItem(rows); err == nil {
			items = append(items, it)
		}
	}
	rows.Close()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, it := range items {
		pm := matchPath(rules, pathSubject(roots[it.LibraryID], it))
		if _, err := tx.Exec(`UPDATE items SET path_match=? WHERE id=?`, mustJSON(pm), it.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
