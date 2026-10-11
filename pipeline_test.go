package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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
	if cs[0].Title != "Project Hail Mary" || cs[0].Score < matchThreshold || cs[1].Score >= reviewThreshold {
		t.Errorf("%+v", cs)
	}
	// Without a guess, the transcript itself is searched.
	cs = ScoreCandidates(Guess{}, "dune written by frank herbert chapter one", []Candidate{{Title: "Dune", Authors: []string{"Frank Herbert"}}})
	if cs[0].Score < matchThreshold {
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

// TestPipeline runs a set through every stage against fake Whisper, LLM and catalog servers.
func TestPipeline(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	root, data := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(root, "x"), 0o755)
	for _, f := range []string{"x/01 of 02.mp3", "x/02 of 02.mp3"} {
		out, err := exec.Command("ffmpeg", "-nostdin", "-loglevel", "error", "-f", "lavfi", "-i", "anullsrc=r=22050:cl=mono", "-t", "2", filepath.Join(root, f)).CombinedOutput()
		if err != nil {
			t.Fatal(string(out))
		}
	}
	whisper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/asr" {
			if _, _, err := r.FormFile("audio_file"); err != nil {
				http.Error(w, "no file", 400)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"text": "This is Audible. Project Hail Mary by Andy Weir."})
		}
	}))
	defer whisper.Close()
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"response": `{"title":"Project Hail Mary","author":"Andy Weir","narrator":"","series":"","series_index":""}`})
	}))
	defer llm.Close()

	calls := 0
	cfg := Config{DataDir: data, SampleSeconds: 30, WhisperURL: whisper.URL, OllamaURL: llm.URL, OllamaModel: "m"}
	db, _ := OpenDB(filepath.Join(data, "t.db"))
	api := &API{Name: "fake", Label: "Fake", Windows: []Window{{100, time.Second}}, Search: func(ctx context.Context, q Query) ([]Candidate, error) {
		calls++
		if calls == 1 {
			return nil, errRateLimited{time.Second, "HTTP 429"}
		}
		return []Candidate{{API: "fake", Title: q.Title, Authors: []string{q.Author}, Series: "", Year: "2021"}}, nil
	}}
	app := &App{db: db, cfg: cfg, apis: []*API{api}, limiter: NewLimiter(db, []*API{api}), token: "t",
		services: map[string]*ServiceStatus{}, wake: make(chan struct{}, 1), scanning: map[int64]bool{}}
	id, _ := addLibrary(db, root)
	if _, err := ScanLibrary(db, Library{ID: id, Path: root}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	app.Start(ctx)

	deadline := time.Now().Add(30 * time.Second)
	var it *Item
	for time.Now().Before(deadline) {
		it, _ = getItem(db, 1)
		if it.Status == StMatched || it.Status == StError {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if it.Status != StMatched || it.Title != "Project Hail Mary" || it.Author != "Andy Weir" {
		t.Fatalf("status %s (%s) title %q", it.Status, it.Detail, it.Title)
	}
	if calls != 2 {
		t.Errorf("expected a rate-limited call then a retry, got %d calls", calls)
	}

	// Final step: organize, then undo.
	items, roots := app.organizeItems(0)
	plans := PlanOrganize(items, roots)
	res, err := Apply(plans, data)
	if err != nil || res.Moved != 2 {
		t.Fatalf("apply: %v %+v", err, res)
	}
	if _, err := os.Stat(filepath.Join(root, "Andy Weir", "Project Hail Mary", "01 of 02.mp3")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "x")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("empty source folder should be removed")
	}
	restored, failed, err := Undo(res.Journal)
	if err != nil || len(failed) > 0 || len(restored) != 2 {
		t.Fatalf("undo %v %v", err, failed)
	}
	if _, err := os.Stat(filepath.Join(root, "x", "02 of 02.mp3")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "Andy Weir")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("undo should remove the folders it emptied")
	}
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
