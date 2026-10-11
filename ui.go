package main

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
)

// Stages shown on the Queue tab, in pipeline order.
var queueStages = []string{StPendingSample, StPendingTranscript, StPendingExtract, StPendingLookup, StReview, StNoMatch, StError}

var completedStatuses = []string{StMatched, StOrganized}

// Row is one set as shown in the Queue, Completed and All tabs.
type Row struct {
	ID                    int64
	Full, Parent, Name    string
	Files                 []string
	Kind                  string
	Status, Detail        string
	Author, Title, Series string
	Index                 string
	Link, LinkLabel       string
	Methods               string
	PathAgrees            string // "Yes", "No", or "" when no path rule matched / nothing identified
	PathTitle, PathAuthor string
	Priority              int64
	Missing, Identified   bool
}

func toRow(it *Item, region string) Row {
	r := Row{ID: it.ID, Full: it.Folder, Name: filepath.Base(it.Folder), Parent: filepath.Base(filepath.Dir(it.Folder)),
		Files: it.Files, Kind: it.Kind, Status: it.Status, Detail: it.Detail, Priority: it.Priority, Missing: it.Missing,
		PathTitle: it.PathMatch.Title, PathAuthor: it.PathMatch.Author}
	r.Identified = it.Status == StMatched || it.Status == StOrganized
	if r.Identified {
		r.Author, r.Title, r.Series, r.Index = it.Author, it.Title, it.Series, it.SeriesIndex
		switch {
		case it.ASIN != "":
			if region == "" {
				region = "com"
			}
			r.Link, r.LinkLabel = "https://www.audible."+region+"/pd/"+it.ASIN, "ASIN "+it.ASIN
		case it.ISBN != "":
			r.Link, r.LinkLabel = "https://openlibrary.org/isbn/"+it.ISBN, "ISBN "+it.ISBN
		case it.Link != "":
			r.Link, r.LinkLabel = it.Link, "catalog page"
		}
		if it.PathMatch.Title != "" {
			agree := dice(it.PathMatch.Title, it.Title) >= 0.5 &&
				(it.PathMatch.Author == "" || it.Author == "" || dice(it.PathMatch.Author, it.Author) >= 0.5)
			r.PathAgrees = map[bool]string{true: "Yes", false: "No"}[agree]
		}
	}
	// Methods used: every method that listened, then whatever settled it.
	var used []string
	add := func(s string) {
		if s != "" && !contains1(used, s) {
			used = append(used, s)
		}
	}
	for _, s := range it.Samples {
		add(methodLabel(s.Method))
	}
	if r.Identified {
		add(it.FoundBy)
	}
	r.Methods = strings.Join(used, " → ")
	return r
}

func (a *App) queuePage(w http.ResponseWriter, r *http.Request, render func(http.ResponseWriter, string, map[string]any)) {
	q := r.URL.Query()
	stage := q.Get("stage")
	lib, _ := strconv.ParseInt(q.Get("lib"), 10, 64)
	stages := queueStages
	if contains1(queueStages, stage) {
		stages = []string{stage}
	} else {
		stage = ""
	}
	where := []string{`kind != ?`, `missing = 0`, `status IN (` + placeholders(len(stages)) + `)`}
	args := []any{KindNotASet}
	for _, s := range stages {
		args = append(args, s)
	}
	if lib != 0 {
		where, args = append(where, `library_id = ?`), append(args, lib)
	}
	if text := strings.TrimSpace(q.Get("q")); text != "" {
		where, args = append(where, `folder LIKE ?`), append(args, "%"+text+"%")
	}
	page, _ := strconv.Atoi(q.Get("page"))
	page = max(page, 0)
	var total int
	a.db.QueryRow(`SELECT COUNT(*) FROM items WHERE `+strings.Join(where, " AND "), args...).Scan(&total)
	rows, err := a.db.Query(`SELECT `+itemCols+` FROM items WHERE `+strings.Join(where, " AND ")+
		` ORDER BY priority DESC, id LIMIT ? OFFSET ?`, append(args, pageSize, page*pageSize)...)
	var list []Row
	if err == nil {
		for rows.Next() {
			if it, err := scanItem(rows); err == nil {
				list = append(list, toRow(it, a.cfg.AudibleRegion))
			}
		}
		rows.Close()
	}
	counts := statusCounts(a.db, lib)
	inQueue := 0
	for _, s := range queueStages {
		inQueue += counts[s]
	}
	a.mu.Lock()
	scanning := len(a.scanning) > 0
	a.mu.Unlock()
	render(w, "queue.html", map[string]any{"Rows": list, "Total": total, "Stage": stage, "Stages": queueStages,
		"Counts": counts, "InQueue": inQueue, "Lib": lib, "Q": q.Get("q"), "Page": page, "Offset": page * pageSize,
		"HasMore": (page+1)*pageSize < total, "Scanning": scanning, "Msg": q.Get("msg")})
}

// tablePage renders Completed or All. Sorting and per-column filters run in
// the browser, so the whole list is sent (capped to keep pages responsive).
func (a *App) tablePage(w http.ResponseWriter, r *http.Request, render func(http.ResponseWriter, string, map[string]any), which string) {
	q := r.URL.Query()
	lib, _ := strconv.ParseInt(q.Get("lib"), 10, 64)
	where, args := []string{"1=1"}, []any{}
	if which == "completed" {
		where = append(where, `status IN (`+placeholders(len(completedStatuses))+`)`)
		for _, s := range completedStatuses {
			args = append(args, s)
		}
	}
	if lib != 0 {
		where, args = append(where, `library_id = ?`), append(args, lib)
	}
	const limit = 20000
	var total int
	a.db.QueryRow(`SELECT COUNT(*) FROM items WHERE `+strings.Join(where, " AND "), args...).Scan(&total)
	rows, err := a.db.Query(`SELECT `+itemCols+` FROM items WHERE `+strings.Join(where, " AND ")+` ORDER BY folder LIMIT ?`, append(args, limit)...)
	var list []Row
	if err == nil {
		for rows.Next() {
			if it, err := scanItem(rows); err == nil {
				list = append(list, toRow(it, a.cfg.AudibleRegion))
			}
		}
		rows.Close()
	}
	render(w, "table.html", map[string]any{"Rows": list, "Which": which, "Lib": lib, "Total": total,
		"Capped": total > limit, "Limit": limit, "Msg": q.Get("msg"), "Page_": which})
}

func (a *App) scanAllHandler(r *http.Request) (string, string) {
	libs := listLibraries(a.db)
	if len(libs) == 0 {
		return "/settings#libraries", "Add a library folder first."
	}
	for _, l := range libs {
		a.scanAsync(l.ID)
	}
	return "/queue", fmt.Sprintf("Scanning %d library folder(s) for sets. New sets appear here as they are found.", len(libs))
}

// topHandler moves a set to the front of the queue.
func (a *App) topHandler(r *http.Request) (string, string) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var top int64
	a.db.QueryRow(`SELECT COALESCE(MAX(priority), 0) FROM items`).Scan(&top)
	if _, err := a.db.Exec(`UPDATE items SET priority=?, next_at=0, updated=? WHERE id=?`, top+1, now(), id); err != nil {
		return "", err.Error()
	}
	a.poke()
	return "", "Moved to the top of the queue."
}

// removeLibraryHandler forgets a library and its sets. Files on disk are not touched.
func (a *App) removeLibraryHandler(r *http.Request) (string, string) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if r.FormValue("confirm") != "on" {
		return "/settings#libraries", "Tick the box next to Remove to confirm."
	}
	tx, err := a.db.Begin()
	if err != nil {
		return "/settings#libraries", err.Error()
	}
	defer tx.Rollback()
	tx.Exec(`DELETE FROM jobs WHERE item_id IN (SELECT id FROM items WHERE library_id=?)`, id)
	tx.Exec(`DELETE FROM items WHERE library_id=?`, id)
	tx.Exec(`DELETE FROM libraries WHERE id=?`, id)
	if err := tx.Commit(); err != nil {
		return "/settings#libraries", err.Error()
	}
	return "/settings#libraries", "Library removed from the app. No files were touched."
}
