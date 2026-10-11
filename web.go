package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed templates/*.html
var templateFS embed.FS

var statusLabels = map[string]string{
	"pending":           "Pending recognition",
	StPendingSample:     "Waiting for sample",
	StPendingTranscript: "Waiting for transcript",
	StPendingExtract:    "Reading transcript",
	StPendingLookup:     "Waiting for catalog search",
	StMatched:           "Matched",
	StReview:            "Needs review",
	StNoMatch:           "No match",
	StNotASet:           "Not a set",
	StError:             "Error",
	StOrganized:         "Organized",
	"all":               "All",
}

var filterOrder = []string{"pending", StReview, StMatched, StNoMatch, StError, StNotASet, StOrganized, "all"}

func newToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (a *App) templates() *template.Template {
	return template.Must(template.New("").Funcs(template.FuncMap{
		"label": func(s string) string {
			if l, ok := statusLabels[s]; ok {
				return l
			}
			return s
		},
		"rel": func(root, p string) string {
			if r, err := filepath.Rel(root, p); err == nil && !strings.HasPrefix(r, "..") {
				return r
			}
			return p
		},
		"pct":     func(f float64) string { return fmt.Sprintf("%.0f%%", f*100) },
		"mb":      func(n int64) string { return fmt.Sprintf("%.1f MB", float64(n)/1e6) },
		"ago":     ago,
		"join":    strings.Join,
		"short":   func(s string, n int) string { return short(s, n) },
		"add":     func(a, b int) int { return a + b },
		"json":    func(v any) template.JS { b, _ := json.Marshal(v); return template.JS(b) },
		"qs":      qs,
		"pending": func(s string) bool { return strings.HasPrefix(s, "pending_") },
		"method":  methodLabel,
		"has":     contains1,
		"statusOptions": func() []string {
			var out []string
			for _, s := range append(append([]string{}, queueStages...), StMatched, StOrganized, StNotASet) {
				out = append(out, statusLabels[s])
			}
			return out
		},
		"secs": func(f float64) string { return fmt.Sprintf("%d:%02d", int(f)/60, int(f)%60) },
	}).ParseFS(templateFS, "templates/*.html"))
}

func short(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func ago(t int64) string {
	if t == 0 {
		return "never"
	}
	d := time.Since(time.Unix(t, 0)).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// qs builds a query string from key/value pairs, dropping empty values.
func qs(kv ...any) string {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		if s := fmt.Sprint(kv[i+1]); s != "" && s != "0" {
			v.Set(fmt.Sprint(kv[i]), s)
		}
	}
	if len(v) == 0 {
		return ""
	}
	return "?" + v.Encode()
}

func (a *App) Routes() http.Handler {
	t := a.templates()
	render := func(w http.ResponseWriter, name string, data map[string]any) {
		data["Token"] = a.token
		data["Status"] = a.statusSnapshot()
		data["Libraries"] = listLibraries(a.db)
		if _, ok := data["Page_"]; !ok {
			data["Page_"] = strings.TrimSuffix(name, ".html")
		}
		data["Reload_"] = name == "queue.html" || name == "lookups.html"
		if err := t.ExecuteTemplate(w, name, data); err != nil {
			log.Printf("render %s: %v", name, err)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/queue?"+r.URL.RawQuery, http.StatusFound)
	})
	mux.HandleFunc("GET /queue", func(w http.ResponseWriter, r *http.Request) { a.queuePage(w, r, render) })
	mux.HandleFunc("GET /completed", func(w http.ResponseWriter, r *http.Request) { a.tablePage(w, r, render, "completed") })
	mux.HandleFunc("GET /all", func(w http.ResponseWriter, r *http.Request) { a.tablePage(w, r, render, "all") })
	mux.HandleFunc("GET /lookups", func(w http.ResponseWriter, r *http.Request) { a.lookupsPage(w, r, render) })
	mux.HandleFunc("POST /scan", a.post(a.scanAllHandler))
	mux.HandleFunc("POST /item/{id}/top", a.post(a.topHandler))
	mux.HandleFunc("POST /libraries/{id}/remove", a.post(a.removeLibraryHandler))
	mux.HandleFunc("GET /item/{id}", func(w http.ResponseWriter, r *http.Request) { a.itemPage(w, r, render) })
	mux.HandleFunc("GET /organize", func(w http.ResponseWriter, r *http.Request) { a.organizePage(w, r, render) })
	mux.HandleFunc("GET /sample/{id}/{n}", a.serveSample)
	mux.HandleFunc("GET /settings", func(w http.ResponseWriter, r *http.Request) { a.settingsPage(w, r, render) })
	mux.HandleFunc("POST /settings", a.post(a.settingsHandler))
	mux.HandleFunc("GET /status.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(a.statusSnapshot())
	})
	mux.HandleFunc("POST /libraries", a.post(a.addLibraryHandler))
	mux.HandleFunc("POST /libraries/{id}/scan", a.post(a.scanHandler))
	mux.HandleFunc("POST /item/{id}/accept", a.post(a.acceptHandler))
	mux.HandleFunc("POST /item/{id}/edit", a.post(a.editHandler))
	mux.HandleFunc("POST /item/{id}/retry", a.post(a.retryHandler))
	mux.HandleFunc("POST /item/{id}/usepath", a.post(a.usePathHandler))
	mux.HandleFunc("POST /organize", a.post(a.organizeHandler))
	mux.HandleFunc("POST /undo", a.post(a.undoHandler))
	return mux
}

// post checks the form token, runs h, and redirects back with its message.
func (a *App) post(h func(r *http.Request) (back, msg string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("token") != a.token {
			http.Error(w, "this form is stale; reload the page", http.StatusForbidden)
			return
		}
		back, msg := h(r)
		if back == "" {
			back = r.FormValue("back")
		}
		if back == "" || !strings.HasPrefix(back, "/") || strings.HasPrefix(back, "//") {
			back = "/"
		}
		if msg != "" {
			back, frag, _ := strings.Cut(back, "#")
			sep := "?"
			if strings.Contains(back, "?") {
				sep = "&"
			}
			back += sep + "msg=" + url.QueryEscape(msg)
			if frag != "" {
				back += "#" + frag
			}
			http.Redirect(w, r, back, http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, back, http.StatusSeeOther)
	}
}

// --- status bar data ---

type APIStatus struct {
	Name, Label, Why, LastError string
	ReadyAt, LastOK             int64 // unix ms
	Usage                       []string
	Queued, Running, Failed     int
}

type StatusSnapshot struct {
	Now      int64 // unix ms
	Services []ServiceStatus
	APIs     []APIStatus
	Counts   map[string]int
	Scanning bool
}

func (a *App) statusSnapshot() StatusSnapshot {
	t := time.Now()
	s := StatusSnapshot{Now: t.UnixMilli(), Counts: statusCounts(a.db, 0)}
	a.mu.Lock()
	for _, name := range []string{"whisper", "llm"} {
		if st := a.services[name]; st != nil {
			s.Services = append(s.Services, *st)
		} else {
			s.Services = append(s.Services, ServiceStatus{Name: name, Detail: "checking…"})
		}
	}
	s.Scanning = len(a.scanning) > 0
	a.mu.Unlock()
	for _, api := range a.apis {
		ready, why := a.limiter.ReadyAt(api, t)
		lastErr, lastOK := a.limiter.Info(api.Name)
		as := APIStatus{Name: api.Name, Label: api.Label, Why: why, ReadyAt: ready.UnixMilli(), Usage: a.limiter.Usage(api, t), LastError: lastErr}
		if !lastOK.IsZero() {
			as.LastOK = lastOK.UnixMilli()
		}
		rows, err := a.db.Query(`SELECT status, COUNT(*) FROM jobs WHERE api=? GROUP BY status`, api.Name)
		if err == nil {
			for rows.Next() {
				var st string
				var n int
				rows.Scan(&st, &n)
				switch st {
				case "queued":
					as.Queued = n
				case "running":
					as.Running = n
				case "failed":
					as.Failed = n
				}
			}
			rows.Close()
		}
		s.APIs = append(s.APIs, as)
	}
	return s
}

// --- pages ---

const pageSize = 100

func (a *App) lookupsPage(w http.ResponseWriter, r *http.Request, render func(http.ResponseWriter, string, map[string]any)) {
	q := r.URL.Query()
	st, api := q.Get("status"), q.Get("api")
	if st == "" {
		st = "queued"
	}
	where, args := []string{"1=1"}, []any{}
	if st != "all" {
		where, args = append(where, "j.status=?"), append(args, st)
	}
	if api != "" {
		where, args = append(where, "j.api=?"), append(args, api)
	}
	rows, err := a.db.Query(`SELECT j.id, j.item_id, j.api, j.method, j.round, j.status, j.query, j.attempts, j.next_at, j.error, j.result, j.created, j.updated, i.folder
		FROM jobs j JOIN items i ON i.id=j.item_id WHERE `+strings.Join(where, " AND ")+` ORDER BY j.next_at, j.id LIMIT 500`, args...)
	var jobs []Job
	if err == nil {
		for rows.Next() {
			var j Job
			var qstr, res string
			rows.Scan(&j.ID, &j.ItemID, &j.API, &j.Method, &j.Round, &j.Status, &qstr, &j.Attempts, &j.NextAt, &j.Error, &res, &j.Created, &j.Updated, &j.Folder)
			jsonUnmarshal(qstr, &j.Query)
			var cs []Candidate
			jsonUnmarshal(res, &cs)
			j.Results = len(cs)
			jobs = append(jobs, j)
		}
		rows.Close()
	}
	counts := map[string]int{}
	cr, err := a.db.Query(`SELECT status, COUNT(*) FROM jobs GROUP BY status`)
	if err == nil {
		for cr.Next() {
			var s string
			var n int
			cr.Scan(&s, &n)
			counts[s] = n
			counts["all"] += n
		}
		cr.Close()
	}
	render(w, "lookups.html", map[string]any{"Jobs": jobs, "JobStatus": st, "API": api, "APIs": a.apis,
		"JobCounts": counts, "JobFilters": []string{"queued", "running", "failed", "done", "all"}, "Msg": q.Get("msg"), "NowUnix": now()})
}

func (a *App) itemPage(w http.ResponseWriter, r *http.Request, render func(http.ResponseWriter, string, map[string]any)) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	it, err := getItem(a.db, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var root string
	a.db.QueryRow(`SELECT path FROM libraries WHERE id=?`, it.LibraryID).Scan(&root)
	var target string
	if it.Author != "" && it.Title != "" {
		target = TargetDir(root, it)
	}
	render(w, "item.html", map[string]any{"Item": it, "Root": root, "Target": target, "Msg": r.URL.Query().Get("msg"),
		"MethodLabel": methodLabel(it.Method)})
}

func (a *App) serveSample(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	n, _ := strconv.Atoi(r.PathValue("n"))
	it, err := getItem(a.db, id)
	if err != nil || n < 0 || n >= len(it.Samples) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	http.ServeFile(w, r, it.Samples[n].Path)
}

func (a *App) organizeItems(lib int64) ([]*Item, map[int64]string) {
	items, _, _ := listItems(a.db, ItemFilter{Status: StMatched, LibraryID: lib, Limit: 100000})
	roots := map[int64]string{}
	for _, l := range listLibraries(a.db) {
		roots[l.ID] = l.Path
	}
	return items, roots
}

func (a *App) organizePage(w http.ResponseWriter, r *http.Request, render func(http.ResponseWriter, string, map[string]any)) {
	lib, _ := strconv.ParseInt(r.URL.Query().Get("lib"), 10, 64)
	items, roots := a.organizeItems(lib)
	plans := PlanOrganize(items, roots)
	var ready, files int
	var bytes int64
	for _, p := range plans {
		if p.Ready() {
			ready++
			files += len(p.Moves)
			for _, m := range p.Moves {
				bytes += m.Bytes
			}
		}
	}
	render(w, "organize.html", map[string]any{"Plans": plans, "Ready": ready, "Files": files, "Bytes": bytes,
		"Lib": lib, "Journals": Journals(a.cfg.DataDir), "Msg": r.URL.Query().Get("msg")})
}

// --- actions ---

func (a *App) addLibraryHandler(r *http.Request) (string, string) {
	id, err := addLibrary(a.db, r.FormValue("path"))
	if err != nil {
		return "", err.Error()
	}
	a.scanAsync(id)
	return "/settings#libraries", "Library added; scanning it now."
}

func (a *App) scanHandler(r *http.Request) (string, string) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	a.scanAsync(id)
	return "", "Scanning. Counts update when it finishes."
}

func (a *App) scanAsync(id int64) {
	a.mu.Lock()
	if a.scanning[id] {
		a.mu.Unlock()
		return
	}
	a.scanning[id] = true
	a.mu.Unlock()
	go func() {
		defer func() {
			a.mu.Lock()
			delete(a.scanning, id)
			a.mu.Unlock()
		}()
		for _, l := range listLibraries(a.db) {
			if l.ID == id {
				sum, err := ScanLibrary(a.db, l)
				if err != nil {
					log.Printf("scan %s: %v", l.Path, err)
				} else {
					log.Printf("scan %s: %s", l.Path, sum)
				}
				if err := applyPathRules(a.db, a.settings(), l.ID); err != nil {
					log.Printf("path rules: %v", err)
				}
			}
		}
	}()
}

func (a *App) acceptHandler(r *http.Request) (string, string) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	it, err := getItem(a.db, id)
	if err != nil {
		return "/", "item not found"
	}
	n, err := strconv.Atoi(r.FormValue("candidate"))
	if err != nil || n < 0 || n >= len(it.Candidates) {
		return "", "pick one of the listed matches"
	}
	c := it.Candidates[n]
	if err := a.acceptCandidate(it, c, "user", StMatched, "confirmed by you: "+c.API, nil); err != nil {
		return "", err.Error()
	}
	return fmt.Sprintf("/item/%d", id), "Saved. It is now matched and will appear in the final organize step."
}

func (a *App) editHandler(r *http.Request) (string, string) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	f := func(k string) string { return strings.Join(strings.Fields(r.FormValue(k)), " ") }
	if f("author") == "" || f("title") == "" {
		return "", "author and title are both needed"
	}
	_, err := a.db.Exec(`UPDATE items SET author=?, title=?, series=?, series_index=?, narrator=?, status=?, detail=?, source='manual', found_by='Entered by you', updated=? WHERE id=?`,
		f("author"), f("title"), f("series"), f("series_index"), f("narrator"), StMatched, "entered by you", now(), id)
	if err != nil {
		return "", err.Error()
	}
	return fmt.Sprintf("/item/%d", id), "Saved as matched."
}

func (a *App) retryHandler(r *http.Request) (string, string) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	it, err := getItem(a.db, id)
	if err != nil {
		return "/", "item not found"
	}
	if it.Kind == KindNotASet {
		return "", "this folder is not a set, so it isn't recognized"
	}
	back := fmt.Sprintf("/item/%d", id)
	switch r.FormValue("from") {
	case "restart":
		a.db.Exec(`DELETE FROM jobs WHERE item_id=?`, id)
		a.db.Exec(`UPDATE items SET method='', samples='[]', transcript='', guess='{}', candidates='[]', score=0,
			status=?, detail='starting over', attempts=0, next_at=0, updated=? WHERE id=?`, StPendingSample, now(), id)
	case "text_search":
		var keep []SampleInfo
		for _, s := range it.Samples {
			if s.Method != "text_search" {
				keep = append(keep, s)
			}
		}
		a.db.Exec(`DELETE FROM jobs WHERE item_id=? AND method='text_search'`, id)
		a.db.Exec(`UPDATE items SET method='text_search', samples=?, status=?, detail='text search requested', attempts=0, next_at=0, updated=? WHERE id=?`,
			mustJSON(keep), StPendingSample, now(), id)
	case "lookup":
		g := Guess{Title: strings.TrimSpace(r.FormValue("title")), Author: strings.TrimSpace(r.FormValue("author")), Method: "manual"}
		if g.Title == "" {
			return "", "enter at least a title"
		}
		it.Method = "announce"
		if err := a.queueLookups(it, announceMethod{}, g, Query{Title: g.Title, Author: g.Author}); err != nil {
			return "", err.Error()
		}
	default:
		return "", "unknown step"
	}
	return back, "Queued."
}

func (a *App) settingsPage(w http.ResponseWriter, r *http.Request, render func(http.ResponseWriter, string, map[string]any)) {
	var apis []map[string]any
	s := a.settings()
	for _, api := range a.apis {
		apis = append(apis, map[string]any{"Name": api.Name, "Label": api.Label, "Limit": s.Limits[api.Name]})
	}
	// Editor rows: the rules plus three blank ones for new rules.
	type ruleRow struct {
		I    int
		Rule PathRule
	}
	var rows []ruleRow
	for i, r := range s.PathRules {
		rows = append(rows, ruleRow{i, r})
	}
	for i := len(s.PathRules); i < len(s.PathRules)+3; i++ {
		rows = append(rows, ruleRow{i, PathRule{Enabled: true}})
	}
	var test *PathMatch
	if t := r.URL.Query().Get("test"); t != "" {
		if rules, err := compilePathRules(s.PathRules); err == nil {
			pm := matchPath(rules, filepath.ToSlash(t))
			test = &pm
		}
	}
	render(w, "settings.html", map[string]any{"S": s, "APIList": apis, "Msg": r.URL.Query().Get("msg"),
		"RuleRows": rows, "Test": test, "TestPath": r.URL.Query().Get("test"),
		"AnnounceCatalogs": methodCatalogs["announce"], "TextCatalogs": methodCatalogs["text_search"],
		"HasLLM": a.cfg.OllamaURL != "", "LLMModel": a.cfg.OllamaModel})
}

func (a *App) settingsHandler(r *http.Request) (string, string) {
	r.ParseForm()
	num := func(k string) int { n, _ := strconv.Atoi(strings.TrimSpace(r.FormValue(k))); return n }
	on := func(k string) bool { return r.FormValue(k) == "on" }
	pick := func(k string, allowed []string) []string {
		var out []string
		for _, v := range r.Form[k] {
			if contains1(allowed, v) {
				out = append(out, v)
			}
		}
		return out
	}
	s := a.settings()
	s.Announce = AnnounceSettings{Enabled: on("a_enabled"), Start: num("a_start"), Seconds: num("a_seconds"), UseLLM: on("a_llm"),
		Catalogs: pick("a_catalogs", methodCatalogs["announce"]), MatchPct: num("a_match"), ReviewPct: num("a_review")}
	s.TextSearch = TextSearchSettings{Enabled: on("t_enabled"), Seconds: num("t_seconds"), MiddlePct: num("t_middle"),
		ExtraSamples: num("t_extra"), MaxWords: num("t_words"), Catalogs: pick("t_catalogs", methodCatalogs["text_search"]), AcceptSingle: on("t_single")}
	s.WhisperLanguage = strings.TrimSpace(r.FormValue("language"))
	limits := map[string]string{}
	for _, api := range a.apis {
		limits[api.Name] = strings.ReplaceAll(r.FormValue("limit_"+api.Name), " ", "")
	}
	s.Limits = limits
	type ordered struct {
		order int
		rule  PathRule
	}
	var rs []ordered
	for i := 0; i < 200; i++ {
		pat := strings.TrimSpace(r.FormValue(fmt.Sprintf("rule_pattern_%d", i)))
		if _, present := r.Form[fmt.Sprintf("rule_order_%d", i)]; !present {
			break
		}
		if pat == "" {
			continue // a blank pattern deletes the rule
		}
		name := strings.TrimSpace(r.FormValue(fmt.Sprintf("rule_name_%d", i)))
		if name == "" {
			name = fmt.Sprintf("Rule %d", len(rs)+1)
		}
		order, err := strconv.Atoi(strings.TrimSpace(r.FormValue(fmt.Sprintf("rule_order_%d", i))))
		if err != nil {
			order = 1000 + i
		}
		rs = append(rs, ordered{order*1000 + i, PathRule{Name: name, Pattern: pat, Enabled: on(fmt.Sprintf("rule_enabled_%d", i))}})
	}
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].order < rs[j].order })
	s.PathRules = make([]PathRule, 0, len(rs))
	for _, o := range rs {
		s.PathRules = append(s.PathRules, o.rule)
	}
	s.PathAccept = on("path_accept")
	s.PathCrossCheck = on("path_cross")
	if err := s.validate(); err != nil {
		return "/settings", "Not saved: " + err.Error()
	}
	if err := saveSettings(a.db, s); err != nil {
		return "/settings", err.Error()
	}
	a.applySettings(s)
	go func() {
		if err := applyPathRules(a.db, s, 0); err != nil {
			log.Printf("path rules: %v", err)
		}
	}()
	return "/settings", "Saved. Path rules are being re-applied to every book; other changes apply from each book's next step."
}

// applySettings makes new settings live, including API limits.
func (a *App) applySettings(s Settings) {
	a.mu.Lock()
	a.cur = s
	a.mu.Unlock()
	for _, api := range a.apis {
		if spec, ok := s.Limits[api.Name]; ok && spec != "" {
			if m, err := parseLimits(api.Name + "=" + spec); err == nil {
				a.limiter.SetWindows(api.Name, m[api.Name])
			}
		}
	}
}

func (a *App) organizeHandler(r *http.Request) (string, string) {
	lib, _ := strconv.ParseInt(r.FormValue("lib"), 10, 64)
	items, roots := a.organizeItems(lib)
	plans := PlanOrganize(items, roots)
	files := 0
	for _, p := range plans {
		if p.Ready() {
			files += len(p.Moves)
		}
	}
	if r.FormValue("confirm") != "on" {
		return "", "tick the box to confirm"
	}
	if strconv.Itoa(files) != r.FormValue("expect") {
		return "", "the plan changed since you looked at it; check it again"
	}
	res, err := Apply(plans, a.cfg.DataDir)
	if err != nil {
		return "", err.Error()
	}
	for _, p := range plans {
		for _, id := range res.Items {
			if p.Item.ID == id {
				a.db.Exec(`UPDATE items SET folder=?, status=?, detail=?, updated=? WHERE id=?`,
					p.TargetDir, StOrganized, "moved from "+p.Item.Folder, now(), id)
			}
		}
	}
	msg := fmt.Sprintf("Moved %d files for %d books.", res.Moved, len(res.Items))
	if len(res.Failed) > 0 {
		msg += fmt.Sprintf(" %d files failed and stayed put: %s", len(res.Failed), strings.Join(res.Failed, "; "))
	}
	return "/organize" + qs("lib", lib), msg
}

func (a *App) undoHandler(r *http.Request) (string, string) {
	j := r.FormValue("journal")
	known := false
	for _, k := range Journals(a.cfg.DataDir) {
		known = known || k == j
	}
	if !known {
		return "/organize", "unknown journal"
	}
	restored, failed, err := Undo(j)
	if err != nil {
		return "/organize", err.Error()
	}
	done := map[int64]string{}
	for _, e := range restored {
		done[e.Item] = e.Folder
	}
	for id, folder := range done {
		a.db.Exec(`UPDATE items SET folder=?, status=?, detail='organize undone', updated=? WHERE id=?`, folder, StMatched, now(), id)
	}
	msg := fmt.Sprintf("Undo put back %d files.", len(restored))
	if len(failed) > 0 {
		msg += fmt.Sprintf(" %d could not be restored: %s", len(failed), strings.Join(failed, "; "))
	}
	return "/organize", msg
}

// rescanLoop keeps libraries current.
func (a *App) rescanLoop(ctx context.Context, every time.Duration) {
	for {
		for _, l := range listLibraries(a.db) {
			a.scanAsync(l.ID)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

func methodLabel(name string) string {
	if m := methodByName(name); m != nil {
		return m.Label()
	}
	return ""
}

func (a *App) usePathHandler(r *http.Request) (string, string) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	it, err := getItem(a.db, id)
	if err != nil {
		return "/", "item not found"
	}
	pm := it.PathMatch
	if pm.Author == "" || pm.Title == "" {
		return "", "the path rule did not give both an author and a title"
	}
	_, err = a.db.Exec(`UPDATE items SET status=?, detail=?, author=?, title=?, series=?, series_index=?, narrator=?, year=?,
		source='path', found_by='Path rule', updated=? WHERE id=?`, StMatched, "from path rule \""+pm.Rule+"\"",
		pm.Author, pm.Title, pm.Series, pm.SeriesIndex, pm.Narrator, pm.Year, now(), id)
	if err != nil {
		return "", err.Error()
	}
	return fmt.Sprintf("/item/%d", id), "Saved the path details as matched."
}
