package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
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

	mu       sync.Mutex
	services map[string]*ServiceStatus
	wake     chan struct{}
	scanning map[int64]bool
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
			ORDER BY next_at, id LIMIT 1`, status, now()))
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

func (a *App) doSample(ctx context.Context, it *Item) error {
	wav, err := ExtractSample(ctx, a.cfg, it)
	if err != nil {
		return err
	}
	_, err = a.db.Exec(`UPDATE items SET sample=?, status=?, detail='', attempts=0, next_at=0, updated=? WHERE id=?`,
		wav, StPendingTranscript, now(), it.ID)
	return err
}

func (a *App) doTranscribe(ctx context.Context, it *Item) error {
	text, err := Transcribe(ctx, a.cfg, it.Sample)
	if err != nil {
		var down errServiceDown
		if errors.As(err, &down) {
			a.setService("whisper", false, err.Error())
		}
		return err
	}
	a.setService("whisper", true, "transcribing")
	if strings.TrimSpace(text) == "" {
		_, err = a.db.Exec(`UPDATE items SET transcript='', status=?, detail=?, attempts=0, updated=? WHERE id=?`,
			StNoMatch, "no speech heard in the sample", now(), it.ID)
		return err
	}
	_, err = a.db.Exec(`UPDATE items SET transcript=?, status=?, detail='', attempts=0, next_at=0, updated=? WHERE id=?`,
		text, StPendingExtract, now(), it.ID)
	return err
}

func (a *App) doExtract(ctx context.Context, it *Item) error {
	g, err := ExtractGuess(ctx, a.cfg, it.Transcript)
	if err != nil {
		var down errServiceDown
		if errors.As(err, &down) {
			a.setService("llm", false, err.Error())
		}
		return err
	}
	if a.cfg.OllamaURL != "" {
		a.setService("llm", true, "model "+a.cfg.OllamaModel)
	}
	return a.queueLookups(it.ID, g, it.Transcript)
}

// queueLookups stores the guess and queues one search per API.
func (a *App) queueLookups(id int64, g Guess, transcript string) error {
	q := Query{Title: g.Title, Author: g.Author}
	if q.Title == "" {
		// Nothing clear was extracted; search on the first words heard.
		words := strings.Fields(transcript)
		q.Keywords = strings.Join(words[:min(len(words), 12)], " ")
	}
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	tx.Exec(`DELETE FROM jobs WHERE item_id=? AND status IN ('queued','failed')`, id)
	for _, api := range a.apis {
		if _, err := tx.Exec(`INSERT INTO jobs(item_id, api, query, status, created, updated) VALUES(?,?,?,?,?,?)`,
			id, api.Name, mustJSON(q), "queued", now(), now()); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE items SET guess=?, candidates='[]', status=?, detail='', attempts=0, next_at=0, updated=? WHERE id=?`,
		mustJSON(g), StPendingLookup, now(), id); err != nil {
		return err
	}
	return tx.Commit()
}

type Job struct {
	ID, ItemID       int64
	API, Status      string
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
			WHERE j.api=? AND j.status='queued' AND j.next_at<=? AND i.status=? ORDER BY j.next_at, j.id LIMIT 1`,
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
		t := time.Now()
		a.limiter.Record(api, t)
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
			log.Printf("score item %d: %v", itemID, err)
		}
	}
}

// finishIfDone scores an item once none of its searches are outstanding.
func (a *App) finishIfDone(itemID int64) error {
	var open int
	a.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE item_id=? AND status IN ('queued','running')`, itemID).Scan(&open)
	if open > 0 {
		return nil
	}
	it, err := getItem(a.db, itemID)
	if err != nil || it.Status != StPendingLookup {
		return err
	}
	rows, err := a.db.Query(`SELECT status, result, error FROM jobs WHERE item_id=?`, itemID)
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
	all = ScoreCandidates(it.Guess, it.Transcript, all)
	status, detail := StNoMatch, "no catalog entry resembles what was heard"
	if len(failures) > 0 && len(all) == 0 {
		status, detail = StError, "every search failed: "+strings.Join(failures, "; ")
	}
	var best Candidate
	if len(all) > 0 {
		best = all[0]
		// A different book scoring almost as well means we can't be sure.
		rival := false
		for _, c := range all[1:] {
			if nameKey(c.Title) != nameKey(best.Title) && best.Score-c.Score < 0.1 {
				rival = true
				break
			}
		}
		switch {
		case best.Score >= matchThreshold && !rival:
			status, detail = StMatched, fmt.Sprintf("matched on %s (%.0f%%)", best.API, best.Score*100)
		case best.Score >= reviewThreshold:
			status, detail = StReview, fmt.Sprintf("best guess %.0f%% on %s; please confirm", best.Score*100, best.API)
		}
	}
	if status == StMatched {
		return a.acceptCandidate(it, best, "auto", status, detail, all)
	}
	_, err = a.db.Exec(`UPDATE items SET candidates=?, score=?, status=?, detail=?, updated=? WHERE id=?`,
		mustJSON(all), best.Score, status, detail, now(), itemID)
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
	_, err := a.db.Exec(`UPDATE items SET candidates=?, score=?, status=?, detail=?, author=?, title=?, series=?, series_index=?,
		narrator=?, year=?, source=?, updated=? WHERE id=?`,
		mustJSON(all), c.Score, status, detail, author, c.Title, series, idx, narr, c.Year, source+":"+c.API+":"+c.ID, now(), it.ID)
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
			a.setService("llm", true, "off: using pattern matching on the transcript")
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
