package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Item statuses. The first four are "pending recognition".
const (
	StPendingSample     = "pending_sample"
	StPendingTranscript = "pending_transcript"
	StPendingExtract    = "pending_extract"
	StPendingLookup     = "pending_lookup"
	StMatched           = "matched"
	StReview            = "review"
	StNoMatch           = "no_match"
	StNotASet           = "not_a_set"
	StError             = "error"
	StOrganized         = "organized"
)

var pendingStatuses = []string{StPendingSample, StPendingTranscript, StPendingExtract, StPendingLookup}

const schema = `
PRAGMA journal_mode=WAL;
CREATE TABLE IF NOT EXISTS libraries (
	id INTEGER PRIMARY KEY,
	path TEXT NOT NULL UNIQUE,
	added INTEGER NOT NULL,
	last_scan INTEGER NOT NULL DEFAULT 0,
	scan_summary TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS items (
	id INTEGER PRIMARY KEY,
	library_id INTEGER NOT NULL REFERENCES libraries(id),
	folder TEXT NOT NULL UNIQUE,
	kind TEXT NOT NULL,
	total INTEGER NOT NULL DEFAULT 0,
	files TEXT NOT NULL DEFAULT '[]',
	bytes INTEGER NOT NULL DEFAULT 0,
	reason TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL,
	detail TEXT NOT NULL DEFAULT '',
	attempts INTEGER NOT NULL DEFAULT 0,
	next_at INTEGER NOT NULL DEFAULT 0,
	sample TEXT NOT NULL DEFAULT '',
	transcript TEXT NOT NULL DEFAULT '',
	guess TEXT NOT NULL DEFAULT '{}',
	candidates TEXT NOT NULL DEFAULT '[]',
	score REAL NOT NULL DEFAULT 0,
	author TEXT NOT NULL DEFAULT '',
	title TEXT NOT NULL DEFAULT '',
	series TEXT NOT NULL DEFAULT '',
	series_index TEXT NOT NULL DEFAULT '',
	narrator TEXT NOT NULL DEFAULT '',
	year TEXT NOT NULL DEFAULT '',
	source TEXT NOT NULL DEFAULT '',
	missing INTEGER NOT NULL DEFAULT 0,
	created INTEGER NOT NULL,
	updated INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS items_status ON items(status, next_at);
CREATE TABLE IF NOT EXISTS jobs (
	id INTEGER PRIMARY KEY,
	item_id INTEGER NOT NULL REFERENCES items(id),
	api TEXT NOT NULL,
	query TEXT NOT NULL,
	status TEXT NOT NULL,
	attempts INTEGER NOT NULL DEFAULT 0,
	next_at INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT '',
	result TEXT NOT NULL DEFAULT '[]',
	created INTEGER NOT NULL,
	updated INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS jobs_queue ON jobs(api, status, next_at);
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS api_calls (api TEXT NOT NULL, at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS api_calls_idx ON api_calls(api, at);
CREATE TABLE IF NOT EXISTS api_state (
	api TEXT PRIMARY KEY,
	cooldown_until INTEGER NOT NULL DEFAULT 0,
	strikes INTEGER NOT NULL DEFAULT 0,
	last_error TEXT NOT NULL DEFAULT '',
	last_ok INTEGER NOT NULL DEFAULT 0
);
`

func OpenDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one writer; keeps SQLite simple and safe
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("init database: %w", err)
	}
	// Columns added after the first release.
	for _, m := range []struct{ table, col, def string }{
		{"items", "method", "TEXT NOT NULL DEFAULT ''"},
		{"items", "samples", "TEXT NOT NULL DEFAULT '[]'"},
		{"items", "durations", "TEXT NOT NULL DEFAULT '[]'"},
		{"items", "path_match", "TEXT NOT NULL DEFAULT '{}'"},
		{"items", "priority", "INTEGER NOT NULL DEFAULT 0"},
		{"items", "asin", "TEXT NOT NULL DEFAULT ''"},
		{"items", "isbn", "TEXT NOT NULL DEFAULT ''"},
		{"items", "link", "TEXT NOT NULL DEFAULT ''"},
		{"items", "found_by", "TEXT NOT NULL DEFAULT ''"},
		{"jobs", "method", "TEXT NOT NULL DEFAULT ''"},
		{"jobs", "round", "INTEGER NOT NULL DEFAULT 0"},
	} {
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, m.table, m.col).Scan(&n)
		if n == 0 {
			if _, err := db.Exec(`ALTER TABLE ` + m.table + ` ADD COLUMN ` + m.col + ` ` + m.def); err != nil {
				return nil, fmt.Errorf("migrate %s.%s: %w", m.table, m.col, err)
			}
		}
	}
	return db, nil
}

func now() int64 { return time.Now().Unix() }

type Library struct {
	ID          int64
	Path        string
	LastScan    int64
	ScanSummary string
}

type Guess struct {
	Title       string `json:"title"`
	Author      string `json:"author"`
	Narrator    string `json:"narrator"`
	Series      string `json:"series"`
	SeriesIndex string `json:"series_index"`
	Method      string `json:"method"` // "llm" or "heuristic"
}

type Candidate struct {
	API         string   `json:"api"`
	ID          string   `json:"id"`
	URL         string   `json:"url"`
	Title       string   `json:"title"`
	Subtitle    string   `json:"subtitle,omitempty"`
	Authors     []string `json:"authors"`
	Narrators   []string `json:"narrators,omitempty"`
	Series      string   `json:"series,omitempty"`
	SeriesIndex string   `json:"series_index,omitempty"`
	Year        string   `json:"year,omitempty"`
	ASIN        string   `json:"asin,omitempty"`
	ISBN        string   `json:"isbn,omitempty"`
	Score       float64  `json:"score"`
}

type Item struct {
	ID, LibraryID                                      int64
	Folder, Kind                                       string
	Total                                              int
	Files                                              []string
	Bytes                                              int64
	Reason, Status, Detail                             string
	Attempts                                           int
	NextAt                                             int64
	Sample, Transcript                                 string
	Guess                                              Guess
	Candidates                                         []Candidate
	Score                                              float64
	Author, Title, Series, SeriesIndex, Narrator, Year string
	Source                                             string
	Missing                                            bool
	Method                                             string
	Samples                                            []SampleInfo
	Durations                                          []float64
	PathMatch                                          PathMatch
	Priority                                           int64
	ASIN, ISBN, Link, FoundBy                          string
	Created, Updated                                   int64
}

const itemCols = `id, library_id, folder, kind, total, files, bytes, reason, status, detail, attempts, next_at,
	sample, transcript, guess, candidates, score, author, title, series, series_index, narrator, year, source, missing, created, updated,
	method, samples, durations, path_match, priority, asin, isbn, link, found_by`

type scanner interface{ Scan(...any) error }

func scanItem(r scanner) (*Item, error) {
	var it Item
	var files, guess, cands, samples, durs, pm string
	err := r.Scan(&it.ID, &it.LibraryID, &it.Folder, &it.Kind, &it.Total, &files, &it.Bytes, &it.Reason, &it.Status,
		&it.Detail, &it.Attempts, &it.NextAt, &it.Sample, &it.Transcript, &guess, &cands, &it.Score, &it.Author,
		&it.Title, &it.Series, &it.SeriesIndex, &it.Narrator, &it.Year, &it.Source, &it.Missing, &it.Created, &it.Updated,
		&it.Method, &samples, &durs, &pm, &it.Priority, &it.ASIN, &it.ISBN, &it.Link, &it.FoundBy)
	if err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(files), &it.Files)
	json.Unmarshal([]byte(guess), &it.Guess)
	json.Unmarshal([]byte(cands), &it.Candidates)
	json.Unmarshal([]byte(samples), &it.Samples)
	json.Unmarshal([]byte(durs), &it.Durations)
	json.Unmarshal([]byte(pm), &it.PathMatch)
	return &it, nil
}

func getItem(db *sql.DB, id int64) (*Item, error) {
	return scanItem(db.QueryRow(`SELECT `+itemCols+` FROM items WHERE id=?`, id))
}

func jsonUnmarshal(s string, v any) { json.Unmarshal([]byte(s), v) }

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// setStatus moves an item to a new status, resetting the retry counters.
func setStatus(db *sql.DB, id int64, status, detail string) error {
	_, err := db.Exec(`UPDATE items SET status=?, detail=?, attempts=0, next_at=0, updated=? WHERE id=?`, status, detail, now(), id)
	return err
}

type ItemFilter struct {
	Status    string // a status, "pending", "recognized" or "" for all
	LibraryID int64
	Query     string
	Limit     int
	Offset    int
}

func (f ItemFilter) where() (string, []any) {
	var conds []string
	var args []any
	switch f.Status {
	case "", "all":
	case "pending":
		conds = append(conds, `status IN (`+placeholders(len(pendingStatuses))+`)`)
		for _, s := range pendingStatuses {
			args = append(args, s)
		}
	default:
		conds = append(conds, `status = ?`)
		args = append(args, f.Status)
	}
	if f.LibraryID != 0 {
		conds = append(conds, `library_id = ?`)
		args = append(args, f.LibraryID)
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		conds = append(conds, `(folder LIKE ? OR title LIKE ? OR author LIKE ? OR transcript LIKE ? OR path_match LIKE ?)`)
		like := "%" + q + "%"
		args = append(args, like, like, like, like, like)
	}
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func listItems(db *sql.DB, f ItemFilter) ([]*Item, int, error) {
	w, args := f.where()
	var total int
	if err := db.QueryRow(`SELECT COUNT(*) FROM items`+w, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if f.Limit == 0 {
		f.Limit = 100
	}
	rows, err := db.Query(`SELECT `+itemCols+` FROM items`+w+` ORDER BY folder LIMIT ? OFFSET ?`, append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, it)
	}
	return out, total, rows.Err()
}

func statusCounts(db *sql.DB, libraryID int64) map[string]int {
	q, args := `SELECT status, COUNT(*) FROM items`, []any{}
	if libraryID != 0 {
		q += ` WHERE library_id=?`
		args = append(args, libraryID)
	}
	rows, err := db.Query(q+` GROUP BY status`, args...)
	out := map[string]int{}
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		var n int
		rows.Scan(&s, &n)
		out[s] = n
		out["all"] += n
		for _, p := range pendingStatuses {
			if s == p {
				out["pending"] += n
			}
		}
	}
	return out
}

func listLibraries(db *sql.DB) []Library {
	rows, err := db.Query(`SELECT id, path, last_scan, scan_summary FROM libraries ORDER BY path`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Library
	for rows.Next() {
		var l Library
		rows.Scan(&l.ID, &l.Path, &l.LastScan, &l.ScanSummary)
		out = append(out, l)
	}
	return out
}
