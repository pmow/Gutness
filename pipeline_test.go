package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLimiterWindows(t *testing.T) {
	db, _ := OpenDB(filepath.Join(t.TempDir(), "t.db"))
	api := &API{Name: "x", Windows: []Window{{1, 2 * time.Second}, {3, time.Hour}}}
	l := NewLimiter(db, []*API{api})
	t0 := time.Unix(1_000_000, 0)
	if r, _ := l.ReadyAt(api, t0); !r.Equal(t0) {
		t.Fatal("fresh API should be ready")
	}
	l.Record(api, t0)
	if r, why := l.ReadyAt(api, t0); !r.Equal(t0.Add(2*time.Second)) || why == "" {
		t.Fatalf("after one call: ready %v (%s)", r.Sub(t0), why)
	}
	l.Record(api, t0.Add(2*time.Second))
	l.Record(api, t0.Add(4*time.Second))
	if r, _ := l.ReadyAt(api, t0.Add(10*time.Second)); !r.Equal(t0.Add(time.Hour)) {
		t.Fatalf("hourly window full: ready after %v, want 1h", r.Sub(t0))
	}
	// Survives a restart.
	l2 := NewLimiter(db, []*API{api})
	l2.calls["x"] = l.calls["x"] // stored times are wall-clock; reuse in-memory copy for the fixed clock
	l.Penalize(api, t0, 0, "429")
	if r, _ := l.ReadyAt(api, t0); !r.Equal(t0.Add(time.Hour)) {
		t.Fatalf("penalty shorter than window should not shorten the wait: %v", r.Sub(t0))
	}
	l.Penalize(api, t0, 2*time.Hour, "429")
	if r, why := l.ReadyAt(api, t0); !r.Equal(t0.Add(2*time.Hour)) || why == "" {
		t.Fatalf("Retry-After should win: %v", r.Sub(t0))
	}
}

func TestParseLimits(t *testing.T) {
	m, err := parseLimits("audible=2/1s,100/1h")
	if err != nil || len(m["audible"]) != 2 || m["audible"][1].N != 100 || len(m["openlibrary"]) == 0 {
		t.Fatalf("%v %v", m, err)
	}
	if _, err := parseLimits("audible=lots"); err == nil {
		t.Fatal("bad spec accepted")
	}
}

func TestHeuristicGuess(t *testing.T) {
	g := heuristicGuess("This is Audible. The Way of Kings. Book one of the Stormlight Archive. By Brandon Sanderson. Narrated by Michael Kramer.")
	if g.Title != "The Way of Kings" || g.Author != "Brandon Sanderson" || g.Narrator != "Michael Kramer" || g.SeriesIndex != "1" || g.Series != "Stormlight Archive" {
		t.Errorf("%+v", g)
	}
	g = heuristicGuess("Project Hail Mary, by Andy Weir. Read by Ray Porter.")
	if g.Title != "Project Hail Mary" || g.Author != "Andy Weir" || g.Narrator != "Ray Porter" {
		t.Errorf("%+v", g)
	}
	g = heuristicGuess("Macmillan Audio presents: Dune, by Frank Herbert")
	if g.Title != "Dune" || g.Author != "Frank Herbert" || g.Narrator != "" {
		t.Errorf("%+v", g)
	}
	if g = heuristicGuess("The Present, by Spencer Johnson."); g.Title != "The Present" {
		t.Errorf("%+v", g)
	}
	g = heuristicGuess("Leviathan Wakes. Book one of the Expanse, by James S. A. Corey. Narrated by Jefferson Mays.")
	if g.Title != "Leviathan Wakes" || g.Author != "James S. A. Corey" {
		t.Errorf("%+v", g)
	}
}

func TestScoring(t *testing.T) {
	cs := ScoreCandidates(Guess{Title: "Project Hail Mary", Author: "Andy Weir"}, "", []Candidate{
		{API: "openlibrary", Title: "The Martian", Authors: []string{"Andy Weir"}},
		{API: "audible", Title: "Project Hail Mary", Authors: []string{"Andy Weir"}},
	})
	if cs[0].Title != "Project Hail Mary" || cs[0].Score < 0.8 || cs[1].Score >= 0.45 {
		t.Errorf("%+v", cs)
	}
	// Without a guess, the transcript itself is searched.
	cs = ScoreCandidates(Guess{}, "dune written by frank herbert chapter one", []Candidate{{Title: "Dune", Authors: []string{"Frank Herbert"}}})
	if cs[0].Score < 0.8 {
		t.Errorf("%+v", cs)
	}
}

func TestScanLibrary(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"A/01 of 03.mp3", "A/02 of 03.mp3", "A/03 of 03.mp3", "A/cover.jpg", "B/x.m4b", "C/1.mp3", "C/2.mp3", ".hidden/1 of 1.mp3"} {
		os.MkdirAll(filepath.Join(root, filepath.Dir(f)), 0o755)
		os.WriteFile(filepath.Join(root, f), []byte("x"), 0o644)
	}
	db, _ := OpenDB(filepath.Join(t.TempDir(), "t.db"))
	id, err := addLibrary(db, root)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := ScanLibrary(db, Library{ID: id, Path: root})
	if err != nil {
		t.Fatal(err)
	}
	if sum.AudioFiles != 6 || sum.Sets != 1 || sum.Singles != 1 || sum.NotSets != 1 || sum.SetFiles+1+sum.NotSetFiles != sum.AudioFiles {
		t.Fatalf("%+v", sum)
	}
	c := statusCounts(db, 0)
	if c["pending"] != 2 || c[StNotASet] != 1 {
		t.Fatalf("counts %v", c)
	}
	// Rescanning an unchanged library changes nothing; removing a folder marks it gone.
	os.RemoveAll(filepath.Join(root, "B"))
	sum, _ = ScanLibrary(db, Library{ID: id, Path: root})
	if sum.New != 0 || sum.Changed != 0 || sum.Missing != 1 {
		t.Fatalf("rescan %+v", sum)
	}
}

func makeAudio(t *testing.T, path string, seconds int) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	out, err := exec.Command("ffmpeg", "-nostdin", "-loglevel", "error", "-f", "lavfi", "-i", "anullsrc=r=22050:cl=mono",
		"-t", strconv.Itoa(seconds), path).CombinedOutput()
	if err != nil {
		t.Fatal(string(out))
	}
}

// TestPipeline runs two books through both methods with fake speech and catalogs:
// "hail" announces its title; "quiet" doesn't, so text search takes over,
// finds two books, listens again, and narrows to one.
func TestPipeline(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	root, data := t.TempDir(), t.TempDir()
	makeAudio(t, filepath.Join(root, "hail", "01 of 02.mp3"), 20)
	makeAudio(t, filepath.Join(root, "hail", "02 of 02.mp3"), 20)
	makeAudio(t, filepath.Join(root, "quiet", "1-2.mp3"), 60)
	makeAudio(t, filepath.Join(root, "quiet", "2-2.mp3"), 60)

	db, _ := OpenDB(filepath.Join(data, "t.db"))
	var searches []Query
	api := &API{Name: "googlebooks", Label: "Fake", Windows: []Window{{100, time.Second}}, Search: func(ctx context.Context, q Query) ([]Candidate, error) {
		searches = append(searches, q)
		if len(searches) == 1 {
			return nil, errRateLimited{time.Second, "HTTP 429"}
		}
		switch {
		case q.Title != "":
			return []Candidate{{API: "googlebooks", Title: q.Title, Authors: []string{q.Author}, Year: "2021"}}, nil
		case strings.Contains(q.FullText, "second stretch"):
			return []Candidate{{API: "googlebooks", Title: "Leviathan Wakes", Authors: []string{"James S. A. Corey"}}}, nil
		default:
			return []Candidate{{API: "googlebooks", Title: "Leviathan Wakes", Authors: []string{"James S. A. Corey"}},
				{API: "googlebooks", Title: "Caliban's War", Authors: []string{"James S. A. Corey"}}}, nil
		}
	}}
	app := &App{db: db, cfg: Config{DataDir: data}, apis: []*API{api}, limiter: NewLimiter(db, []*API{api}), token: "t",
		services: map[string]*ServiceStatus{}, wake: make(chan struct{}, 1), scanning: map[int64]bool{}}
	s := defaultSettings(Config{})
	s.Announce.Catalogs = []string{"googlebooks"}
	s.TextSearch.Catalogs = []string{"googlebooks"}
	app.applySettings(s)
	var mu sync.Mutex
	heard := map[string]int{}
	app.transcribe = func(ctx context.Context, lang, path string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		heard[filepath.Base(path)]++
		switch {
		case strings.Contains(path, "/samples/"+itemIDFor(db, root, "hail")+"-"):
			return "Penguin Random House presents: Project Hail Mary, by Andy Weir, read by Ray Porter.", nil
		case strings.HasSuffix(path, "-1.wav"):
			return "Chapter one. It was a dark and stormy night.", nil
		case strings.HasSuffix(path, "-2.wav"):
			return "the ships drifted past the ring gate while holden watched the screens in silence", nil
		default:
			return "a second stretch of words from deeper in the book about the protomolecule", nil
		}
	}
	id, _ := addLibrary(db, root)
	if _, err := ScanLibrary(db, Library{ID: id, Path: root}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	app.Start(ctx)

	wait := func(folder string) *Item {
		deadline := time.Now().Add(40 * time.Second)
		for {
			var iid int64
			db.QueryRow(`SELECT id FROM items WHERE folder=?`, filepath.Join(root, folder)).Scan(&iid)
			it, _ := getItem(db, iid)
			if it != nil && (it.Status == StMatched || it.Status == StError || it.Status == StNoMatch || it.Status == StReview) {
				return it
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s stuck in %s (%s)", folder, it.Status, it.Detail)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	hail := wait("hail")
	if hail.Status != StMatched || hail.Title != "Project Hail Mary" || hail.Author != "Andy Weir" || hail.Method != "announce" {
		t.Fatalf("hail: %s (%s) %q by %q via %s", hail.Status, hail.Detail, hail.Title, hail.Author, hail.Method)
	}
	if len(hail.Samples) != 1 || hail.Samples[0].Seconds != 15 || hail.Samples[0].Offset != 0 {
		t.Errorf("announcement sample: %+v", hail.Samples)
	}
	quiet := wait("quiet")
	if quiet.Status != StMatched || quiet.Title != "Leviathan Wakes" || quiet.Method != "text_search" {
		t.Fatalf("quiet: %s (%s) %q via %s", quiet.Status, quiet.Detail, quiet.Title, quiet.Method)
	}
	ts := samplesOf(quiet, "text_search")
	if len(ts) != 2 {
		t.Fatalf("text search should take two samples, took %d", len(ts))
	}
	// Both samples fall in the middle 80% of the 120 s set: 12 s .. 78 s.
	for _, sm := range ts {
		g := globalOffset(quiet, quiet.Durations, sm)
		if g < 11.9 || g > 78.1 || sm.Seconds != 30 {
			t.Errorf("sample at %.1f s (%s+%.1f) for %d s is outside the middle", g, sm.File, sm.Offset, sm.Seconds)
		}
	}
	last := searches[len(searches)-1]
	if !strings.Contains(last.FullText, "ring gate") || !strings.Contains(last.FullText, "second stretch") {
		t.Errorf("second text search should use all words heard: %q", last.FullText)
	}

	// Final step: organize, then undo.
	items, roots := app.organizeItems(0)
	plans := PlanOrganize(items, roots)
	res, err := Apply(plans, data)
	if err != nil || res.Moved != 4 {
		t.Fatalf("apply: %v %+v", err, res)
	}
	if _, err := os.Stat(filepath.Join(root, "Andy Weir", "Project Hail Mary", "01 of 02.mp3")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "hail")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("empty source folder should be removed")
	}
	restored, failed, err := Undo(res.Journal)
	if err != nil || len(failed) > 0 || len(restored) != 4 {
		t.Fatalf("undo %v %v", err, failed)
	}
	if _, err := os.Stat(filepath.Join(root, "Andy Weir")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("undo should remove the folders it emptied")
	}
}

func itemIDFor(db *sql.DB, root, folder string) string {
	var id int64
	db.QueryRow(`SELECT id FROM items WHERE folder=?`, filepath.Join(root, folder)).Scan(&id)
	return strconv.FormatInt(id, 10)
}

func TestTargetDir(t *testing.T) {
	it := &Item{Author: "Brandon Sanderson", Series: "The Stormlight Archive", SeriesIndex: "01", Title: "The Way of Kings"}
	if got := TargetDir("/lib", it); got != "/lib/Brandon Sanderson/The Stormlight Archive/Vol 1 - The Way of Kings" {
		t.Error(got)
	}
	it = &Item{Author: "AC/DC", Title: `What? "Now"`}
	if got := TargetDir("/lib", it); got != "/lib/AC-DC/What- -Now-" {
		t.Error(got)
	}
}
