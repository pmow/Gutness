// Command aural recognizes audiobooks by listening to them: it finds numbered
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
	"strconv"
	"strings"
	"time"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return n
	}
	return def
}

func main() {
	cfg := Config{
		Addr:          env("AURAL_ADDR", ":8078"),
		DataDir:       env("AURAL_DATA", "./data"),
		SampleStart:   envInt("SAMPLE_START", 0),
		SampleSeconds: envInt("SAMPLE_SECONDS", 30),
		WhisperURL:    env("WHISPER_URL", "http://localhost:9000"),
		WhisperLang:   env("WHISPER_LANGUAGE", "en"),
		OllamaURL:     os.Getenv("OLLAMA_URL"),
		OllamaModel:   env("OLLAMA_MODEL", "llama3.2:3b"),
		GoogleKey:     os.Getenv("GOOGLE_BOOKS_API_KEY"),
		AudibleRegion: env("AUDIBLE_REGION", "com"),
		APIs:          strings.Split(env("LOOKUP_APIS", "audible,openlibrary,googlebooks"), ","),
		Limits:        os.Getenv("API_LIMITS"),
	}
	flag.StringVar(&cfg.Addr, "addr", cfg.Addr, "listen address")
	flag.StringVar(&cfg.DataDir, "data", cfg.DataDir, "folder for the database, samples and journals")
	flag.Parse()

	if _, err := exec.LookPath("ffmpeg"); err != nil {
		log.Fatal("ffmpeg is required and was not found on PATH")
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatal(err)
	}
	db, err := OpenDB(filepath.Join(cfg.DataDir, "aural.db"))
	if err != nil {
		log.Fatal(err)
	}
	apis, err := buildAPIs(cfg)
	if err != nil {
		log.Fatal(err)
	}
	app := &App{db: db, cfg: cfg, apis: apis, limiter: NewLimiter(db, apis), token: newToken(),
		services: map[string]*ServiceStatus{}, wake: make(chan struct{}, 1), scanning: map[int64]bool{}}

	app.applySettings(loadSettings(db, cfg))

	// Library folders can be listed up front, e.g. LIBRARIES=/audiobooks/a:/audiobooks/b
	for _, p := range filepath.SplitList(os.Getenv("LIBRARIES")) {
		if p = strings.TrimSpace(p); p != "" {
			if _, err := addLibrary(db, p); err != nil {
				log.Printf("library %s: %v", p, err)
			}
		}
	}
	ctx := context.Background()
	app.Start(ctx)
	go app.rescanLoop(ctx, time.Duration(envInt("RESCAN_HOURS", 6))*time.Hour)

	log.Printf("Aural Organizer listening on %s (data in %s)", cfg.Addr, cfg.DataDir)
	log.Fatal(http.ListenAndServe(cfg.Addr, app.Routes()))
}
