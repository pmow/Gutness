package main

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
)

//go:embed page.html
var pageHTML string

type Edit struct {
	Author, Series, Index, Title string
	Skip                         bool
}

type App struct {
	mu       sync.Mutex
	token    string
	Source   string
	Dest     string
	CopyOnly bool
	Scan     *ScanResult
	edits    map[string]Edit
	msg, err string
	tmpl     *template.Template
}

func NewApp(source, dest string, copyOnly bool) *App {
	b := make([]byte, 16)
	rand.Read(b)
	a := &App{Source: source, Dest: dest, CopyOnly: copyOnly, token: hex.EncodeToString(b), edits: map[string]Edit{}}
	a.tmpl = template.Must(template.New("page").Funcs(template.FuncMap{
		"rel": func(base, p string) string {
			if r, err := filepath.Rel(base, p); err == nil && !strings.HasPrefix(r, "..") {
				return r
			}
			return p
		},
		"mb":   func(n int64) string { return fmt.Sprintf("%.1f MB", float64(n)/1e6) },
		"len3": func(a, b, c int) int { return a + b + c },
		"sub":  func(a, b int) int { return a - b },
		"showTabs": func() map[string]string {
			return map[string]string{"held": "Held for review", "ready": "Ready", "warn": "Worth checking", "all": "All"}
		},
	}).Parse(pageHTML))
	return a
}

// applyEdits overlays the user's corrections on freshly scanned books.
func (a *App) applyEdits() {
	if a.Scan == nil {
		return
	}
	for _, b := range a.Scan.Books {
		e, ok := a.edits[b.ID]
		if !ok {
			continue
		}
		for _, f := range []struct {
			dst  *string
			val  string
			name string
		}{{&b.Author, e.Author, "author"}, {&b.Series, e.Series, "series"}, {&b.SeriesIndex, e.Index, "index"}, {&b.Title, e.Title, "title"}} {
			if *f.dst != f.val {
				*f.dst, b.From[f.name] = f.val, "edited"
			}
		}
		b.Skip = e.Skip
	}
}

func (a *App) book(id string) *Book {
	if a.Scan == nil {
		return nil
	}
	for _, b := range a.Scan.Books {
		if b.ID == id {
			return b
		}
	}
	return nil
}

func (a *App) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.index)
	mux.HandleFunc("POST /scan", a.post(a.scan))
	mux.HandleFunc("POST /book/{id}", a.post(a.saveBook))
	mux.HandleFunc("POST /lookup/{id}", a.post(a.lookup))
	mux.HandleFunc("POST /apply", a.post(a.apply))
	mux.HandleFunc("POST /undo", a.post(a.undo))
	return mux
}

// post checks the form token, runs h under the lock and redirects home.
func (a *App) post(h func(r *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("token") != a.token {
			http.Error(w, "stale or forged form; reload the page", http.StatusForbidden)
			return
		}
		a.mu.Lock()
		a.msg, a.err = "", ""
		h(r)
		a.mu.Unlock()
		back := "/"
		if s := r.FormValue("show"); s != "" {
			back += "?show=" + s
		}
		if id := r.PathValue("id"); id != "" {
			back += "#b" + id
		}
		http.Redirect(w, r, back, http.StatusSeeOther)
	}
}

func (a *App) rescan() {
	s, err := Scan(a.Source)
	if err != nil {
		a.Scan, a.err = nil, err.Error()
		return
	}
	a.Scan = s
	a.applyEdits()
	if err := s.CheckCounts(); err != nil {
		a.err = "count check failed: " + err.Error()
	}
}

func (a *App) scan(r *http.Request) {
	a.Source = strings.TrimSpace(r.FormValue("source"))
	a.Dest = strings.TrimSpace(r.FormValue("dest"))
	a.CopyOnly = r.FormValue("copy") == "on"
	if a.Dest == "" {
		a.Dest = a.Source
	}
	a.rescan()
	if a.Scan != nil && a.err == "" {
		a.msg = fmt.Sprintf("Scanned %s: %d audio files in %d books.", a.Scan.Root, a.Scan.AudioFiles, len(a.Scan.Books))
	}
}

func (a *App) saveBook(r *http.Request) {
	b := a.book(r.PathValue("id"))
	if b == nil {
		a.err = "that book is no longer in the scan; scan again"
		return
	}
	a.edits[b.ID] = Edit{
		Author: clean(r.FormValue("author")), Series: clean(r.FormValue("series")),
		Index: clean(r.FormValue("index")), Title: clean(r.FormValue("title")),
		Skip: r.FormValue("skip") == "on",
	}
	a.applyEdits()
}

func (a *App) lookup(r *http.Request) {
	b := a.book(r.PathValue("id"))
	if b == nil {
		a.err = "that book is no longer in the scan; scan again"
		return
	}
	s, err := LookupOpenLibrary(r.Context(), b.Title, b.Author)
	if err != nil {
		a.err = "Lookup for \"" + b.Title + "\": " + err.Error() + ". Nothing was changed."
		return
	}
	e := Edit{Author: b.Author, Series: b.Series, Index: b.SeriesIndex, Title: b.Title, Skip: b.Skip}
	if e.Author == "" {
		e.Author = s.Author
	}
	if e.Title == "" {
		e.Title = s.Title
	}
	a.edits[b.ID] = e
	a.applyEdits()
	if b.Year == "" && s.Year != "" {
		b.Year, b.From["year"] = s.Year, "online"
	}
	a.msg = fmt.Sprintf("Open Library suggests \"%s\" by %s (%s). Blank fields were filled; edit the row if the rest differs.", s.Title, s.Author, s.Year)
}

func (a *App) apply(r *http.Request) {
	if a.Scan == nil {
		a.err = "scan first"
		return
	}
	if r.FormValue("confirm") != "on" {
		a.err = "tick the confirmation box to apply the plan"
		return
	}
	a.rescan() // the disk may have changed since the preview
	if a.Scan == nil || a.err != "" {
		return
	}
	plan := BuildPlan(a.Scan, a.Dest)
	if fmt.Sprint(plan.FilesReady) != r.FormValue("expect") {
		a.err = "the library changed since you looked at the preview; review the updated plan and apply again"
		return
	}
	res, err := Apply(plan, a.Scan.Root, a.CopyOnly)
	if err != nil {
		a.err = err.Error()
		return
	}
	verb := "Moved"
	if a.CopyOnly {
		verb = "Copied"
	}
	a.msg = fmt.Sprintf("%s %d files for %d books. Journal: %s", verb, res.Moved+res.Copied, res.BooksDone, res.Journal)
	if len(res.Failed) > 0 {
		a.err = fmt.Sprintf("%d files failed and were left in place: %s", len(res.Failed), strings.Join(res.Failed, "; "))
	}
	a.rescan()
}

func (a *App) undo(r *http.Request) {
	j := r.FormValue("journal")
	ok := false
	for _, k := range Journals(a.Dest) {
		ok = ok || k == j
	}
	if !ok {
		a.err = "unknown journal"
		return
	}
	n, failed, err := Undo(j)
	if err != nil {
		a.err = err.Error()
		return
	}
	a.msg = fmt.Sprintf("Undo restored %d files.", n)
	if len(failed) > 0 {
		a.err = fmt.Sprintf("%d files could not be restored: %s", len(failed), strings.Join(failed, "; "))
	}
	if a.Scan != nil {
		a.rescan()
	}
}

type pageData struct {
	A        *App
	Token    string
	Plan     *Plan
	Rows     []BookPlan
	Show     string
	Journals []string
	Msg, Err string
}

func (a *App) index(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	d := pageData{A: a, Token: a.token, Show: r.URL.Query().Get("show"), Msg: a.msg, Err: a.err}
	if d.Show == "" {
		d.Show = "held"
	}
	if a.Scan != nil {
		d.Plan = BuildPlan(a.Scan, a.Dest)
		for _, bp := range d.Plan.Books {
			held := !bp.Ready() && !(bp.InPlace && len(bp.Problems) == 0)
			if d.Show == "all" || (d.Show == "ready" && bp.Ready()) || (d.Show == "held" && held) ||
				(d.Show == "warn" && len(bp.Book.Warnings) > 0) {
				d.Rows = append(d.Rows, bp)
			}
		}
	}
	if a.Dest != "" {
		d.Journals = Journals(a.Dest)
	}
	if err := a.tmpl.Execute(w, d); err != nil {
		http.Error(w, err.Error(), 500)
	}
}
