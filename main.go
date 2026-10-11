// Command gutness recognizes audiobooks by listening to them: it finds numbered
// sets of audio files, transcribes a sample of part 1, looks the result up in
// book catalogs through a rate-limited queue, and finally organizes matched
// books into an Audiobookshelf-style folder layout.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	// Everything else is on the Settings page. GUTNESS_PORT is the only
	// environment variable.
	cfg := Config{Addr: ":" + env("GUTNESS_PORT", "8078"), DataDir: "/data"}
	if _, err := os.Stat(cfg.DataDir); err != nil {
		cfg.DataDir = "./data" // running outside Docker
	}
	flag.StringVar(&cfg.DataDir, "data", cfg.DataDir, "folder for the database, samples and journals")
	flag.Parse()

	if _, err := exec.LookPath("ffmpeg"); err != nil {
		log.Fatal("ffmpeg is required and was not found on PATH")
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatal(err)
	}
	db, err := OpenDB(filepath.Join(cfg.DataDir, "gutness.db"))
	if err != nil {
		log.Fatal(err)
	}
	app := &App{db: db, cfg: cfg, token: newToken(),
		services: map[string]*ServiceStatus{}, wake: make(chan struct{}, 1), recheck: make(chan struct{}, 1), scanning: map[int64]bool{}}
	app.apis = buildAPIs(app.conf)
	app.limiter = NewLimiter(db, app.apis)
	app.applySettings(loadSettings(db))

	ctx := context.Background()
	app.Start(ctx)
	go app.rescanLoop(ctx)

	log.Printf("Gutness listening on %s (data in %s)", cfg.Addr, cfg.DataDir)
	log.Fatal(http.ListenAndServe(cfg.Addr, app.Routes()))
}
