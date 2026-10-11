package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const maxAttempts = 5

type ServiceStatus struct {
	Name, Detail string
	OK           bool
	Checked      time.Time
}

type App struct {
	db      *sql.DB
	cfg     Config
	apis    []*API
	limiter *Limiter
	token   string

	mu  sync.Mutex
	cur Settings // guarded by mu; read with settings()

	// transcribe replaces the Whisper call in tests.
	transcribe func(ctx context.Context, lang, path string) (string, error)
	services   map[string]*ServiceStatus
	wake       chan struct{}
	scanning   map[int64]bool
}

func (a *App) setService(name string, ok bool, detail string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.services[name] = &ServiceStatus{Name: name, OK: ok, Detail: detail, Checked: time.Now()}
}

func (a *App) poke() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// Start runs the recognition pipeline in the background:
// sample -> transcribe -> extract -> lookups (rate-limited per API) -> score.
func (a *App) Start(ctx context.Context) {
	go a.stage(ctx, StPendingSample, a.doSample)
	go a.stage(ctx, StPendingTranscript, a.doTranscribe)
	go a.stage(ctx, StPendingExtract, a.doExtract)
	for _, api := range a.apis {
		go a.lookupLoop(ctx, api)
	}
	go a.watchServices(ctx)
	// Jobs left "running" by a crash go back in the queue.
	a.db.Exec(`UPDATE jobs SET status='queued' WHERE status='running'`)
}

// stage repeatedly takes the next item in status and runs fn on it.
func (a *App) stage(ctx context.Context, status string, fn func(context.Context, *Item) error) {
	for ctx.Err() == nil {
		it, err := scanItem(a.db.QueryRow(`SELECT `+itemCols+` FROM items WHERE status=? AND next_at<=? AND missing=0
			ORDER BY priority DESC, next_at, id LIMIT 1`, status, now()))
		if err != nil {
			if err != sql.ErrNoRows {
				log.Printf("%s: %v", status, err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(3 * time.Second):
			}
			continue
		}
		if err := fn(ctx, it); err != nil {
			var down errServiceDown
			if errors.As(err, &down) {
				// Not the item's fault: wait for the service without using up attempts.
				a.db.Exec(`UPDATE items SET detail=?, next_at=?, updated=? WHERE id=?`, "waiting: "+err.Error(), now()+30, now(), it.ID)
				continue
			}
			it.Attempts++
			if it.Attempts >= maxAttempts {
				a.db.Exec(`UPDATE items SET status=?, detail=?, attempts=?, updated=? WHERE id=?`,
					StError, fmt.Sprintf("%s failed %d times: %v", status, it.Attempts, err), it.Attempts, now(), it.ID)
			} else {
				a.db.Exec(`UPDATE items SET detail=?, attempts=?, next_at=?, updated=? WHERE id=?`,
					fmt.Sprintf("attempt %d failed: %v", it.Attempts, err), it.Attempts, now()+int64(30<<it.Attempts), now(), it.ID)
			}
		}
	}
}

func (a *App) settings() Settings {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cur
}

// doSample starts the item's current method (or the first enabled one) and
// cuts the audio that method asks for.
func (a *App) doSample(ctx context.Context, it *Item) error {
	s := a.settings()
	if pm := it.PathMatch; s.PathAccept && it.Method == "" && pm.Author != "" && pm.Title != "" {
		_, err := a.db.Exec(`UPDATE items SET status=?, detail=?, author=?, title=?, series=?, series_index=?, narrator=?, year=?,
			source='path', found_by='Path rule', attempts=0, updated=? WHERE id=?`, StMatched, "path rule \""+pm.Rule+"\" (listening skipped)",
			pm.Author, pm.Title, pm.Series, pm.SeriesIndex, pm.Narrator, pm.Year, now(), it.ID)
		return err
	}
	m := methodByName(it.Method)
	if m == nil || !m.Enabled(s) {
		if m = nextMethod(s, ""); m == nil {
			return setStatus(a.db, it.ID, StNoMatch, "no recognition methods are enabled (see Settings)")
		}
		it.Method = m.Name()
	}
	if len(it.Files) == 0 {
		return errors.New("item has no files")
	}
	si, err := m.NextSample(ctx, s, it)
	if err != nil {
		return err
	}
	si.Path = filepath.Join(a.cfg.DataDir, "samples", fmt.Sprintf("%d-%d.wav", it.ID, len(it.Samples)+1))
	if err := ExtractSample(ctx, filepath.Join(it.Folder, si.File), si.Offset, si.Seconds, si.Path); err != nil {
		return err
	}
	it.Samples = append(it.Samples, si)
	_, err = a.db.Exec(`UPDATE items SET method=?, samples=?, durations=?, sample=?, status=?, detail='', attempts=0, next_at=0, updated=? WHERE id=?`,
		it.Method, mustJSON(it.Samples), mustJSON(it.Durations), si.Path, StPendingTranscript, now(), it.ID)
	return err
}

// doTranscribe transcribes the newest sample.
func (a *App) doTranscribe(ctx context.Context, it *Item) error {
	if len(it.Samples) == 0 {
		return setStatus(a.db, it.ID, StPendingSample, "")
	}
	last := &it.Samples[len(it.Samples)-1]
	transcribe := a.transcribe
	if transcribe == nil {
		transcribe = func(ctx context.Context, lang, path string) (string, error) {
			return Transcribe(ctx, a.cfg, lang, path)
		}
	}
	text, err := transcribe(ctx, a.settings().WhisperLanguage, last.Path)
	if err != nil {
		var down errServiceDown
		if errors.As(err, &down) {
			a.setService("whisper", false, err.Error())
		}
		return err
	}
	a.setService("whisper", true, "transcribing")
	last.Text = text
	_, err = a.db.Exec(`UPDATE items SET samples=?, transcript=?, status=?, detail='', attempts=0, next_at=0, updated=? WHERE id=?`,
		mustJSON(it.Samples), heardBy(it, it.Method), StPendingExtract, now(), it.ID)
	return err
}

// doExtract lets the method turn what it heard into searches.
func (a *App) doExtract(ctx context.Context, it *Item) error {
	s := a.settings()
	m := methodByName(it.Method)
	if m == nil {
		return setStatus(a.db, it.ID, StPendingSample, "")
	}
	g, q, err := m.Plan(ctx, a, s, it)
	if errors.Is(err, errGiveUp) {
		return a.advance(it, err.Error(), nil)
	}
	if err != nil {
		return err
	}
	return a.queueLookups(it, m, g, q)
}

// advance hands the item to the next enabled method, or ends recognition.
func (a *App) advance(it *Item, reason string, cands []Candidate) error {
	next := nextMethod(a.settings(), it.Method)
	if cands == nil {
		cands = it.Candidates
	}
	if next == nil {
		_, err := a.db.Exec(`UPDATE items SET status=?, detail=?, candidates=?, attempts=0, next_at=0, updated=? WHERE id=?`,
			StNoMatch, reason, mustJSON(cands), now(), it.ID)
		return err
	}
	_, err := a.db.Exec(`UPDATE items SET method=?, status=?, detail=?, candidates=?, attempts=0, next_at=0, updated=? WHERE id=?`,
		next.Name(), StPendingSample, reason+"; trying "+next.Label(), mustJSON(cands), now(), it.ID)
	return err
}

// queueLookups stores the guess and queues one search per catalog the method uses.
func (a *App) queueLookups(it *Item, m Method, g Guess, q Query) error {
	s := a.settings()
	round := len(samplesOf(it, m.Name()))
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	tx.Exec(`DELETE FROM jobs WHERE item_id=? AND method=? AND round=?`, it.ID, m.Name(), round)
	n := 0
	for _, api := range a.apis {
		if !contains1(m.Catalogs(s), api.Name) {
			continue
		}
		n++
		if _, err := tx.Exec(`INSERT INTO jobs(item_id, api, method, round, query, status, created, updated) VALUES(?,?,?,?,?,?,?,?)`,
			it.ID, api.Name, m.Name(), round, mustJSON(q), "queued", now(), now()); err != nil {
			return err
		}
	}
	if n == 0 {
		tx.Rollback()
		return a.advance(it, m.Label()+" has no catalogs selected", nil)
	}
	if _, err := tx.Exec(`UPDATE items SET method=?, guess=?, status=?, detail='', attempts=0, next_at=0, updated=? WHERE id=?`,
		m.Name(), mustJSON(g), StPendingLookup, now(), it.ID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	a.poke()
	return nil
}

type Job struct {
	ID, ItemID       int64
	API, Status      string
	Method           string
	Round            int
	Query            Query
	Attempts         int
	NextAt           int64
	Error            string
	Results          int
	Folder           string
	Created, Updated int64
}

// lookupLoop sends one API's queued searches as fast as its limits allow.
func (a *App) lookupLoop(ctx context.Context, api *API) {
	for ctx.Err() == nil {
		ready, _ := a.limiter.ReadyAt(api, time.Now())
		if wait := time.Until(ready); wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(min(wait, 5*time.Second)):
			}
			continue
		}
		var id, itemID int64
		var qs string
		var attempts int
		err := a.db.QueryRow(`SELECT j.id, j.item_id, j.query, j.attempts FROM jobs j JOIN items i ON i.id=j.item_id
			WHERE j.api=? AND j.status='queued' AND j.next_at<=? AND i.status=? ORDER BY i.priority DESC, j.next_at, j.id LIMIT 1`,
			api.Name, now(), StPendingLookup).Scan(&id, &itemID, &qs, &attempts)
		if err != nil {
			select {
			case <-ctx.Done():
			case <-a.wake:
			case <-time.After(3 * time.Second):
			}
			continue
		}
		var q Query
		jsonUnmarshal(qs, &q)
		a.db.Exec(`UPDATE jobs SET status='running', updated=? WHERE id=?`, now(), id)
		a.limiter.Record(api, time.Now())
		cs, err := api.Search(ctx, q)
		var rl errRateLimited
		switch {
		case errors.As(err, &rl):
			a.limiter.Penalize(api, time.Now(), rl.retryAfter, err.Error())
			a.db.Exec(`UPDATE jobs SET status='queued', error=?, updated=? WHERE id=?`, "rate limited, will retry: "+err.Error(), now(), id)
			continue
		case err != nil:
			attempts++
			a.limiter.Failed(api, err.Error())
			if attempts >= maxAttempts {
				a.db.Exec(`UPDATE jobs SET status='failed', attempts=?, error=?, updated=? WHERE id=?`, attempts, err.Error(), now(), id)
			} else {
				a.db.Exec(`UPDATE jobs SET status='queued', attempts=?, error=?, next_at=?, updated=? WHERE id=?`,
					attempts, err.Error(), now()+int64(60<<attempts), now(), id)
				continue
			}
		default:
			a.limiter.Succeeded(api, time.Now())
			a.db.Exec(`UPDATE jobs SET status='done', error='', result=?, updated=? WHERE id=?`, mustJSON(cs), now(), id)
		}
		if err := a.finishIfDone(itemID); err != nil {
			log.Printf("decide item %d: %v", itemID, err)
		}
	}
}

// finishIfDone lets the method decide once the current round's searches are all back.
func (a *App) finishIfDone(itemID int64) error {
	it, err := getItem(a.db, itemID)
	if err != nil || it.Status != StPendingLookup {
		return err
	}
	m := methodByName(it.Method)
	if m == nil {
		return setStatus(a.db, it.ID, StPendingSample, "")
	}
	round := len(samplesOf(it, it.Method))
	var open int
	a.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE item_id=? AND method=? AND round=? AND status IN ('queued','running')`,
		itemID, it.Method, round).Scan(&open)
	if open > 0 {
		return nil
	}
	rows, err := a.db.Query(`SELECT status, result, error FROM jobs WHERE item_id=? AND method=? AND round=?`, itemID, it.Method, round)
	if err != nil {
		return err
	}
	var all []Candidate
	var failures []string
	for rows.Next() {
		var st, res, e string
		rows.Scan(&st, &res, &e)
		var cs []Candidate
		jsonUnmarshal(res, &cs)
		all = append(all, cs...)
		if st == "failed" {
			failures = append(failures, e)
		}
	}
	rows.Close()
	if len(all) == 0 && len(failures) > 0 {
		_, err = a.db.Exec(`UPDATE items SET status=?, detail=?, updated=? WHERE id=?`,
			StError, "every search failed: "+strings.Join(failures, "; "), now(), itemID)
		return err
	}
	v := m.Decide(a.settings(), it, all)
	switch {
	case v.MoreAudio:
		_, err = a.db.Exec(`UPDATE items SET status=?, detail=?, candidates=?, attempts=0, next_at=0, updated=? WHERE id=?`,
			StPendingSample, v.Detail, mustJSON(v.Candidates), now(), itemID)
		return err
	case v.GiveUp:
		return a.advance(it, v.Detail, v.Candidates)
	case v.Status == StMatched:
		c := v.Candidates[0]
		// A path rule that names a different book is worth a second look.
		if pm := it.PathMatch; a.settings().PathCrossCheck && pm.Title != "" && dice(pm.Title, c.Title) < 0.5 {
			return a.acceptCandidate(it, c, "auto", StReview,
				fmt.Sprintf("listening says %q but the path says %q; please confirm", c.Title, pm.Title), v.Candidates)
		}
		return a.acceptCandidate(it, c, "auto", v.Status, v.Detail, v.Candidates)
	}
	score := 0.0
	if len(v.Candidates) > 0 {
		score = v.Candidates[0].Score
	}
	_, err = a.db.Exec(`UPDATE items SET candidates=?, score=?, status=?, detail=?, updated=? WHERE id=?`,
		mustJSON(v.Candidates), score, v.Status, v.Detail, now(), itemID)
	return err
}

// acceptCandidate copies a catalog entry's details onto the item.
func (a *App) acceptCandidate(it *Item, c Candidate, source, status, detail string, all []Candidate) error {
	if all == nil {
		all = it.Candidates
	}
	author := strings.Join(c.Authors[:min(len(c.Authors), 3)], ", ")
	narr := strings.Join(c.Narrators[:min(len(c.Narrators), 3)], ", ")
	if narr == "" {
		narr = it.Guess.Narrator
	}
	series, idx := c.Series, c.SeriesIndex
	if series == "" {
		series, idx = it.Guess.Series, it.Guess.SeriesIndex
	}
	foundBy := methodLabel(it.Method)
	if source == "user" {
		foundBy = "Picked by you"
	}
	_, err := a.db.Exec(`UPDATE items SET candidates=?, score=?, status=?, detail=?, author=?, title=?, series=?, series_index=?,
		narrator=?, year=?, source=?, asin=?, isbn=?, link=?, found_by=?, updated=? WHERE id=?`,
		mustJSON(all), c.Score, status, detail, author, c.Title, series, idx, narr, c.Year, source+":"+c.API+":"+c.ID,
		c.ASIN, c.ISBN, c.URL, foundBy, now(), it.ID)
	return err
}

// watchServices keeps the Whisper/LLM status current and pulls the model.
func (a *App) watchServices(ctx context.Context) {
	var pulling atomic.Bool
	for ctx.Err() == nil {
		if err := pingService(ctx, a.cfg.WhisperURL, "/docs"); err != nil {
			a.setService("whisper", false, "not reachable: "+err.Error())
		} else {
			a.setService("whisper", true, "ready")
		}
		if a.cfg.OllamaURL == "" {
			a.setService("llm", true, "not configured; announcements are read by pattern matching")
		} else if has, err := ollamaHasModel(ctx, a.cfg); err != nil {
			a.setService("llm", false, "not reachable: "+err.Error())
		} else if !has {
			a.setService("llm", false, "downloading model "+a.cfg.OllamaModel)
			if pulling.CompareAndSwap(false, true) {
				go func() {
					defer pulling.Store(false)
					if err := ollamaPull(a.cfg); err != nil {
						log.Printf("pull %s: %v", a.cfg.OllamaModel, err)
					}
				}()
			}
		} else {
			a.setService("llm", true, "model "+a.cfg.OllamaModel)
		}
		select {
		case <-ctx.Done():
		case <-time.After(20 * time.Second):
		}
	}
}
