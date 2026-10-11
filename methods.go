package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// A Method is one way of recognizing a book. Enabled methods run in order;
// when one gives up, the next starts. Each round of a method is:
// pick a sample -> transcribe it -> plan searches -> run them -> decide.
type Method interface {
	Name() string
	Label() string
	Enabled(s Settings) bool
	Catalogs(s Settings) []string
	// NextSample says which stretch of audio to transcribe next.
	NextSample(ctx context.Context, s Settings, it *Item) (SampleInfo, error)
	// Plan turns what was heard into searches. errGiveUp hands over to the next method.
	Plan(ctx context.Context, a *App, s Settings, it *Item) (Guess, Query, error)
	// Decide looks at the search results.
	Decide(s Settings, it *Item, results []Candidate) Verdict
}

type SampleInfo struct {
	Method  string  `json:"method"`
	File    string  `json:"file"`
	Offset  float64 `json:"offset"`
	Seconds int     `json:"seconds"`
	Path    string  `json:"path"`
	Text    string  `json:"text"`
}

type Verdict struct {
	Status     string // StMatched, StReview, StNoMatch
	Detail     string
	Candidates []Candidate
	MoreAudio  bool // take another sample with the same method
	GiveUp     bool // let the next method try
}

var errGiveUp = errors.New("method gave up")

type giveUp struct{ reason string }

func (g giveUp) Error() string   { return g.reason }
func (g giveUp) Is(t error) bool { return t == errGiveUp }

var methods = []Method{announceMethod{}, textSearchMethod{}}

func methodByName(name string) Method {
	for _, m := range methods {
		if m.Name() == name {
			return m
		}
	}
	return nil
}

// nextMethod returns the first enabled method after current ("" = start).
func nextMethod(s Settings, current string) Method {
	passed := current == ""
	for _, m := range methods {
		if passed && m.Enabled(s) {
			return m
		}
		if m.Name() == current {
			passed = true
		}
	}
	return nil
}

// samplesOf returns the samples taken by one method.
func samplesOf(it *Item, method string) []SampleInfo {
	var out []SampleInfo
	for _, s := range it.Samples {
		if s.Method == method {
			out = append(out, s)
		}
	}
	return out
}

func heardBy(it *Item, method string) string {
	var parts []string
	for _, s := range samplesOf(it, method) {
		if t := strings.TrimSpace(s.Text); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, " ")
}

// --- Method 1: title announcement ---

type announceMethod struct{}

func (announceMethod) Name() string                 { return "announce" }
func (announceMethod) Label() string                { return "Title announcement" }
func (announceMethod) Enabled(s Settings) bool      { return s.Announce.Enabled }
func (announceMethod) Catalogs(s Settings) []string { return s.Announce.Catalogs }

func (announceMethod) NextSample(ctx context.Context, s Settings, it *Item) (SampleInfo, error) {
	return SampleInfo{Method: "announce", File: it.Files[0], Offset: float64(s.Announce.Start), Seconds: s.Announce.Seconds}, nil
}

func (announceMethod) Plan(ctx context.Context, a *App, s Settings, it *Item) (Guess, Query, error) {
	text := heardBy(it, "announce")
	g := heuristicGuess(text)
	if (g.Title == "" || g.Author == "") && s.Announce.UseLLM && a.cfg.OllamaURL != "" {
		lg, err := ExtractGuess(ctx, a.cfg, text)
		if err != nil {
			var down errServiceDown
			if errors.As(err, &down) {
				a.setService("llm", false, err.Error())
			}
			return g, Query{}, err
		}
		a.setService("llm", true, "model "+a.cfg.OllamaModel)
		g = lg
	}
	// The bare minimum is "Title, by Author".
	if g.Title == "" || g.Author == "" {
		return g, Query{}, giveUp{"no \"Title, by Author\" announcement heard in the first " + strconv.Itoa(s.Announce.Seconds) + " seconds"}
	}
	return g, Query{Title: g.Title, Author: g.Author}, nil
}

func (announceMethod) Decide(s Settings, it *Item, results []Candidate) Verdict {
	all := ScoreCandidates(it.Guess, heardBy(it, "announce"), results)
	v := Verdict{Status: StNoMatch, Detail: "no catalog entry matches the announced title", Candidates: all, GiveUp: true}
	if len(all) == 0 {
		return v
	}
	best := all[0]
	rival := false // a different book scoring almost as well
	for _, c := range all[1:] {
		if nameKey(c.Title) != nameKey(best.Title) && best.Score-c.Score < 0.1 {
			rival = true
			break
		}
	}
	match, review := float64(s.Announce.MatchPct)/100, float64(s.Announce.ReviewPct)/100
	switch {
	case best.Score >= match && !rival:
		v = Verdict{Status: StMatched, Detail: fmt.Sprintf("title announcement matched on %s (%.0f%%)", best.API, best.Score*100), Candidates: all}
	case best.Score >= review:
		v = Verdict{Status: StReview, Detail: fmt.Sprintf("announcement best guess %.0f%% on %s; please confirm", best.Score*100, best.API), Candidates: all}
	}
	return v
}

// --- Method 2: text search ---

type textSearchMethod struct{}

func (textSearchMethod) Name() string                 { return "text_search" }
func (textSearchMethod) Label() string                { return "Text search" }
func (textSearchMethod) Enabled(s Settings) bool      { return s.TextSearch.Enabled }
func (textSearchMethod) Catalogs(s Settings) []string { return s.TextSearch.Catalogs }

// NextSample picks a random stretch inside the middle MiddlePct of the whole
// set, away from earlier samples, and maps it to a file and offset.
func (textSearchMethod) NextSample(ctx context.Context, s Settings, it *Item) (SampleInfo, error) {
	durs, err := fileDurations(ctx, it)
	if err != nil {
		return SampleInfo{}, err
	}
	total := 0.0
	for _, d := range durs {
		total += d
	}
	secs := float64(s.TextSearch.Seconds)
	margin := total * float64(100-s.TextSearch.MiddlePct) / 200
	lo, hi := margin, total-margin-secs
	if hi <= lo {
		lo, hi = 0, max(0, total-secs)
	}
	prev := samplesOf(it, "text_search")
	var pos float64
	for try := 0; try < 20; try++ {
		pos = lo + rand.Float64()*(hi-lo)
		clash := false
		for _, p := range prev {
			if abs(globalOffset(it, durs, p)-pos) < secs {
				clash = true
			}
		}
		if !clash {
			break
		}
	}
	// Map the position to a file; keep the sample inside that file.
	for i, d := range durs {
		if pos < d || i == len(durs)-1 {
			off := min(pos, max(0, d-secs))
			return SampleInfo{Method: "text_search", File: it.Files[i], Offset: off, Seconds: s.TextSearch.Seconds}, nil
		}
		pos -= d
	}
	return SampleInfo{}, errors.New("set has no audio")
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func globalOffset(it *Item, durs []float64, s SampleInfo) float64 {
	off := 0.0
	for i, f := range it.Files {
		if f == s.File {
			return off + s.Offset
		}
		off += durs[i]
	}
	return -1e9
}

func (textSearchMethod) Plan(ctx context.Context, a *App, s Settings, it *Item) (Guess, Query, error) {
	words := strings.Fields(heardBy(it, "text_search"))
	if len(words) < 8 {
		return Guess{}, Query{}, giveUp{"too few words heard in the middle of the book to search on"}
	}
	if len(words) > s.TextSearch.MaxWords {
		words = words[:s.TextSearch.MaxWords]
	}
	return Guess{Method: "text"}, Query{FullText: strings.Join(words, " ")}, nil
}

// Decide counts distinct books found. One is a match; two or more asks for
// another sample while extra samples remain; none gives up.
func (textSearchMethod) Decide(s Settings, it *Item, results []Candidate) Verdict {
	type book struct {
		c    Candidate
		hits int
	}
	var order []string
	books := map[string]*book{}
	for _, c := range results {
		author := ""
		if len(c.Authors) > 0 {
			author = c.Authors[0]
		}
		key := nameKey(c.Title) + "|" + nameKey(author)
		if b, ok := books[key]; ok {
			b.hits++
			continue
		}
		books[key] = &book{c: c, hits: 1}
		order = append(order, key)
	}
	var cs []Candidate
	for _, k := range order {
		b := books[k]
		b.c.Score = float64(b.hits) / float64(len(results))
		cs = append(cs, b.c)
	}
	sortByScore(cs)
	rounds := len(samplesOf(it, "text_search"))
	switch {
	case len(cs) == 0:
		return Verdict{Status: StNoMatch, Detail: "no book's text matched the words heard", GiveUp: true}
	case len(cs) == 1:
		cs[0].Score = 1
		if s.TextSearch.AcceptSingle {
			return Verdict{Status: StMatched, Detail: fmt.Sprintf("text search found exactly one book (%d s heard)", rounds*s.TextSearch.Seconds), Candidates: cs}
		}
		return Verdict{Status: StReview, Detail: "text search found one book; please confirm", Candidates: cs}
	case rounds <= s.TextSearch.ExtraSamples:
		return Verdict{Detail: fmt.Sprintf("%d books matched; listening to more", len(cs)), Candidates: cs, MoreAudio: true}
	default:
		return Verdict{Status: StReview, Detail: fmt.Sprintf("%d books matched %d s of text; please pick one", len(cs), rounds*s.TextSearch.Seconds), Candidates: cs}
	}
}

// fileDurations measures each file of the item with ffprobe (cached on the item).
func fileDurations(ctx context.Context, it *Item) ([]float64, error) {
	if len(it.Durations) == len(it.Files) {
		return it.Durations, nil
	}
	var out []float64
	for _, f := range it.Files {
		raw, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_entries", "format=duration",
			"-of", "default=noprint_wrappers=1:nokey=1", filepath.Join(it.Folder, f)).Output()
		if err != nil {
			return nil, fmt.Errorf("ffprobe could not read %s: %v", f, err)
		}
		d, err := strconv.ParseFloat(strings.TrimSpace(string(raw)), 64)
		if err != nil {
			return nil, fmt.Errorf("no duration for %s", f)
		}
		out = append(out, d)
	}
	it.Durations = out
	return out, nil
}
