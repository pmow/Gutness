package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Query struct {
	Title    string `json:"title,omitempty"`
	Author   string `json:"author,omitempty"`
	Keywords string `json:"keywords,omitempty"`
}

type Window struct {
	N   int
	Per time.Duration
}

func (w Window) String() string {
	units := []struct {
		d    time.Duration
		name string
	}{{24 * time.Hour, "day"}, {time.Hour, "hour"}, {time.Minute, "minute"}, {time.Second, "second"}}
	for _, u := range units {
		if w.Per%u.d == 0 {
			if n := int(w.Per / u.d); n > 1 {
				return fmt.Sprintf("%d per %d %ss", w.N, n, u.name)
			}
			return fmt.Sprintf("%d per %s", w.N, u.name)
		}
	}
	return fmt.Sprintf("%d per %s", w.N, w.Per)
}

type API struct {
	Name, Label string
	Windows     []Window
	Search      func(ctx context.Context, q Query) ([]Candidate, error)
}

// errRateLimited is returned when an API says to slow down (HTTP 429/503).
type errRateLimited struct {
	retryAfter time.Duration
	msg        string
}

func (e errRateLimited) Error() string { return e.msg }

// Default request budgets. They are deliberately below what the services
// tolerate; a 429 still triggers a cooldown on top of these.
var defaultLimits = map[string][]Window{
	"audible":     {{1, 2 * time.Second}, {300, time.Hour}},
	"openlibrary": {{1, time.Second}, {500, time.Hour}},
	"googlebooks": {{1, time.Second}, {1000, 24 * time.Hour}},
}

// parseLimits reads overrides like "audible=1/2s,300/1h;googlebooks=900/24h".
func parseLimits(s string) (map[string][]Window, error) {
	out := map[string][]Window{}
	for k, v := range defaultLimits {
		out[k] = v
	}
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, spec, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("bad limit %q", part)
		}
		var ws []Window
		for _, w := range strings.Split(spec, ",") {
			n, per, ok := strings.Cut(strings.TrimSpace(w), "/")
			count, err1 := strconv.Atoi(n)
			dur, err2 := time.ParseDuration(per)
			if !ok || err1 != nil || err2 != nil || count < 1 {
				return nil, fmt.Errorf("bad limit %q (want like 300/1h)", w)
			}
			ws = append(ws, Window{count, dur})
		}
		out[strings.TrimSpace(name)] = ws
	}
	return out, nil
}

func buildAPIs(cfg Config) ([]*API, error) {
	limits, err := parseLimits(cfg.Limits)
	if err != nil {
		return nil, err
	}
	all := map[string]*API{
		"audible":     {Name: "audible", Label: "Audible catalog", Search: audibleSearch(cfg.AudibleRegion)},
		"openlibrary": {Name: "openlibrary", Label: "Open Library", Search: openLibrarySearch},
		"googlebooks": {Name: "googlebooks", Label: "Google Books", Search: googleBooksSearch(cfg.GoogleKey)},
	}
	var out []*API
	for _, name := range cfg.APIs {
		a, ok := all[strings.TrimSpace(name)]
		if !ok {
			return nil, fmt.Errorf("unknown API %q (have audible, openlibrary, googlebooks)", name)
		}
		a.Windows = limits[a.Name]
		out = append(out, a)
	}
	return out, nil
}

// Limiter tracks calls per API, persisted so a restart keeps its budget.
type Limiter struct {
	mu       sync.Mutex
	db       *sql.DB
	calls    map[string][]time.Time
	cooldown map[string]time.Time
	strikes  map[string]int
	lastErr  map[string]string
	lastOK   map[string]time.Time
}

func NewLimiter(db *sql.DB, apis []*API) *Limiter {
	l := &Limiter{db: db, calls: map[string][]time.Time{}, cooldown: map[string]time.Time{},
		strikes: map[string]int{}, lastErr: map[string]string{}, lastOK: map[string]time.Time{}}
	for _, a := range apis {
		longest := time.Duration(0)
		for _, w := range a.Windows {
			if w.Per > longest {
				longest = w.Per
			}
		}
		rows, err := db.Query(`SELECT at FROM api_calls WHERE api=? AND at>=? ORDER BY at`, a.Name, time.Now().Add(-longest).UnixMilli())
		if err == nil {
			for rows.Next() {
				var ms int64
				rows.Scan(&ms)
				l.calls[a.Name] = append(l.calls[a.Name], time.UnixMilli(ms))
			}
			rows.Close()
		}
		var cd, strikes, ok int64
		var lastErr string
		if db.QueryRow(`SELECT cooldown_until, strikes, last_error, last_ok FROM api_state WHERE api=?`, a.Name).Scan(&cd, &strikes, &lastErr, &ok) == nil {
			l.cooldown[a.Name], l.strikes[a.Name], l.lastErr[a.Name] = time.Unix(cd, 0), int(strikes), lastErr
			if ok > 0 {
				l.lastOK[a.Name] = time.Unix(ok, 0)
			}
		}
	}
	return l
}

// ReadyAt says when the next call to a may go out, and why it must wait.
func (l *Limiter) ReadyAt(a *API, t time.Time) (time.Time, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ready, why := t, ""
	if cd := l.cooldown[a.Name]; cd.After(ready) {
		ready, why = cd, "cooling down after the service asked us to slow down"
	}
	calls := l.calls[a.Name]
	for _, w := range a.Windows {
		var in []time.Time
		for _, c := range calls {
			if c.After(t.Add(-w.Per)) {
				in = append(in, c)
			}
		}
		if len(in) >= w.N {
			// Wait until enough calls age out of the window.
			free := in[len(in)-w.N].Add(w.Per)
			if free.After(ready) {
				ready, why = free, "limit of "+w.String()+" reached"
			}
		}
	}
	return ready, why
}

func (l *Limiter) Usage(a *API, t time.Time) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, w := range a.Windows {
		n := 0
		for _, c := range l.calls[a.Name] {
			if c.After(t.Add(-w.Per)) {
				n++
			}
		}
		out = append(out, fmt.Sprintf("%d of %s", n, w))
	}
	return out
}

func (l *Limiter) Record(a *API, t time.Time) {
	l.mu.Lock()
	longest := time.Duration(0)
	for _, w := range a.Windows {
		if w.Per > longest {
			longest = w.Per
		}
	}
	calls := l.calls[a.Name]
	for len(calls) > 0 && calls[0].Before(t.Add(-longest)) {
		calls = calls[1:]
	}
	l.calls[a.Name] = append(calls, t)
	l.mu.Unlock()
	l.db.Exec(`INSERT INTO api_calls(api, at) VALUES(?, ?)`, a.Name, t.UnixMilli())
	l.db.Exec(`DELETE FROM api_calls WHERE api=? AND at<?`, a.Name, t.Add(-longest).UnixMilli())
}

// Penalize starts a cooldown: the service's Retry-After if given, otherwise
// 1, 5, 15, then 60 minutes for repeated rate-limit replies.
func (l *Limiter) Penalize(a *API, t time.Time, retryAfter time.Duration, msg string) {
	l.mu.Lock()
	l.strikes[a.Name]++
	if retryAfter <= 0 {
		steps := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour}
		retryAfter = steps[min(l.strikes[a.Name], len(steps))-1]
	}
	l.cooldown[a.Name] = t.Add(retryAfter)
	l.lastErr[a.Name] = msg
	cd, strikes := l.cooldown[a.Name].Unix(), l.strikes[a.Name]
	l.mu.Unlock()
	l.saveState(a.Name, cd, strikes, msg, 0)
}

func (l *Limiter) Succeeded(a *API, t time.Time) {
	l.mu.Lock()
	l.strikes[a.Name] = 0
	l.lastOK[a.Name] = t
	l.lastErr[a.Name] = ""
	cd := l.cooldown[a.Name].Unix()
	l.mu.Unlock()
	l.saveState(a.Name, cd, 0, "", t.Unix())
}

func (l *Limiter) Failed(a *API, msg string) {
	l.mu.Lock()
	l.lastErr[a.Name] = msg
	l.mu.Unlock()
	l.db.Exec(`INSERT INTO api_state(api, last_error) VALUES(?, ?) ON CONFLICT(api) DO UPDATE SET last_error=excluded.last_error`, a.Name, msg)
}

func (l *Limiter) saveState(api string, cd int64, strikes int, msg string, ok int64) {
	l.db.Exec(`INSERT INTO api_state(api, cooldown_until, strikes, last_error, last_ok) VALUES(?,?,?,?,?)
		ON CONFLICT(api) DO UPDATE SET cooldown_until=excluded.cooldown_until, strikes=excluded.strikes,
		last_error=excluded.last_error, last_ok=MAX(last_ok, excluded.last_ok)`, api, cd, strikes, msg, ok)
}

func (l *Limiter) Info(api string) (lastErr string, lastOK time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastErr[api], l.lastOK[api]
}

// --- clients ---

var httpClient = &http.Client{Timeout: 30 * time.Second}

func getJSON(ctx context.Context, u string, out any) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	req.Header.Set("User-Agent", "aural-audiobook-organizer/0.2 (self-hosted)")
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 429 || resp.StatusCode == 503 {
		ra, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
		return errRateLimited{time.Duration(ra) * time.Second, "HTTP " + resp.Status}
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("HTTP %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
}

func audibleSearch(region string) func(context.Context, Query) ([]Candidate, error) {
	if region == "" {
		region = "com"
	}
	return func(ctx context.Context, q Query) ([]Candidate, error) {
		v := url.Values{"num_results": {"5"}, "products_sort_by": {"Relevance"},
			"response_groups": {"contributors,series,product_attrs,product_desc"}}
		if q.Title != "" {
			v.Set("title", q.Title)
			if q.Author != "" {
				v.Set("author", q.Author)
			}
		} else {
			v.Set("keywords", strings.TrimSpace(q.Author+" "+q.Keywords))
		}
		var out struct {
			Products []struct {
				ASIN, Title, Subtitle string
				ReleaseDate           string `json:"release_date"`
				Authors, Narrators    []struct{ Name string }
				Series                []struct{ Title, Sequence string }
			}
		}
		if err := getJSON(ctx, env("AUDIBLE_BASE_URL", "https://api.audible."+region)+"/1.0/catalog/products?"+v.Encode(), &out); err != nil {
			return nil, err
		}
		var cs []Candidate
		for _, p := range out.Products {
			c := Candidate{API: "audible", ID: p.ASIN, URL: "https://www.audible." + region + "/pd/" + p.ASIN, Title: p.Title, Subtitle: p.Subtitle}
			for _, a := range p.Authors {
				c.Authors = append(c.Authors, a.Name)
			}
			for _, n := range p.Narrators {
				c.Narrators = append(c.Narrators, n.Name)
			}
			if len(p.Series) > 0 {
				c.Series, c.SeriesIndex = p.Series[0].Title, p.Series[0].Sequence
			}
			if len(p.ReleaseDate) >= 4 {
				c.Year = p.ReleaseDate[:4]
			}
			cs = append(cs, c)
		}
		return cs, nil
	}
}

func openLibrarySearch(ctx context.Context, q Query) ([]Candidate, error) {
	v := url.Values{"limit": {"5"}, "fields": {"key,title,subtitle,author_name,first_publish_year"}}
	if q.Title != "" {
		v.Set("title", q.Title)
		if q.Author != "" {
			v.Set("author", q.Author)
		}
	} else {
		v.Set("q", strings.TrimSpace(q.Author+" "+q.Keywords))
	}
	var out struct {
		Docs []struct {
			Key, Title, Subtitle string
			AuthorName           []string `json:"author_name"`
			Year                 int      `json:"first_publish_year"`
		}
	}
	if err := getJSON(ctx, env("OPENLIBRARY_BASE_URL", "https://openlibrary.org")+"/search.json?"+v.Encode(), &out); err != nil {
		return nil, err
	}
	var cs []Candidate
	for _, d := range out.Docs {
		c := Candidate{API: "openlibrary", ID: d.Key, URL: "https://openlibrary.org" + d.Key, Title: d.Title, Subtitle: d.Subtitle, Authors: d.AuthorName}
		if d.Year > 0 {
			c.Year = strconv.Itoa(d.Year)
		}
		cs = append(cs, c)
	}
	return cs, nil
}

func googleBooksSearch(key string) func(context.Context, Query) ([]Candidate, error) {
	return func(ctx context.Context, q Query) ([]Candidate, error) {
		var terms []string
		if q.Title != "" {
			terms = append(terms, `intitle:"`+q.Title+`"`)
			if q.Author != "" {
				terms = append(terms, `inauthor:"`+q.Author+`"`)
			}
		} else {
			terms = append(terms, strings.TrimSpace(q.Author+" "+q.Keywords))
		}
		v := url.Values{"q": {strings.Join(terms, " ")}, "maxResults": {"5"}, "printType": {"books"}}
		if key != "" {
			v.Set("key", key)
		}
		var out struct {
			Items []struct {
				ID         string
				VolumeInfo struct {
					Title, Subtitle, PublishedDate string
					Authors                        []string
				}
			}
		}
		if err := getJSON(ctx, env("GOOGLEBOOKS_BASE_URL", "https://www.googleapis.com")+"/books/v1/volumes?"+v.Encode(), &out); err != nil {
			return nil, err
		}
		var cs []Candidate
		for _, it := range out.Items {
			vi := it.VolumeInfo
			c := Candidate{API: "googlebooks", ID: it.ID, URL: "https://books.google.com/books?id=" + it.ID, Title: vi.Title, Subtitle: vi.Subtitle, Authors: vi.Authors}
			if len(vi.PublishedDate) >= 4 {
				c.Year = vi.PublishedDate[:4]
			}
			cs = append(cs, c)
		}
		return cs, nil
	}
}
