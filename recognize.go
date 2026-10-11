package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// errServiceDown marks failures of a helper service (Whisper, the LLM) that
// should be retried later without counting against the item.
type errServiceDown struct{ err error }

func (e errServiceDown) Error() string { return e.err.Error() }

func serviceDown(format string, a ...any) error { return errServiceDown{fmt.Errorf(format, a...)} }

type Config struct {
	Addr          string
	DataDir       string
	SampleStart   int // seconds into part 1
	SampleSeconds int
	WhisperURL    string
	WhisperLang   string
	OllamaURL     string
	OllamaModel   string
	GoogleKey     string
	AudibleRegion string
	APIs          []string
	Limits        string
}

// ExtractSample cuts SampleSeconds of 16 kHz mono audio from the first file,
// the format Whisper works on natively.
func ExtractSample(ctx context.Context, cfg Config, it *Item) (string, error) {
	if len(it.Files) == 0 {
		return "", errors.New("item has no files")
	}
	src := filepath.Join(it.Folder, it.Files[0])
	dir := filepath.Join(cfg.DataDir, "samples")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	out := filepath.Join(dir, fmt.Sprintf("%d.wav", it.ID))
	cmd := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-ss", fmt.Sprint(cfg.SampleStart), "-t", fmt.Sprint(cfg.SampleSeconds), "-i", src,
		"-vn", "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", out)
	if msg, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("ffmpeg could not read %s: %s", it.Files[0], strings.TrimSpace(string(msg)))
	}
	if st, err := os.Stat(out); err != nil || st.Size() < 1000 {
		return "", fmt.Errorf("ffmpeg produced no audio from %s", it.Files[0])
	}
	return out, nil
}

// Transcribe sends a sample to an openai-whisper-asr-webservice container.
func Transcribe(ctx context.Context, cfg Config, wav string) (string, error) {
	f, err := os.Open(wav)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, _ := mw.CreateFormFile("audio_file", filepath.Base(wav))
	if _, err := io.Copy(part, f); err != nil {
		return "", err
	}
	mw.Close()
	q := url.Values{"task": {"transcribe"}, "output": {"json"}, "encode": {"true"}}
	if cfg.WhisperLang != "" {
		q.Set("language", cfg.WhisperLang)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(cfg.WhisperURL, "/")+"/asr?"+q.Encode(), &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", serviceDown("Whisper is not reachable at %s: %v", cfg.WhisperURL, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 500 {
		return "", serviceDown("Whisper returned %s", resp.Status)
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("Whisper returned %s: %.200s", resp.Status, raw)
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return strings.TrimSpace(string(raw)), nil // plain-text output
	}
	return strings.TrimSpace(out.Text), nil
}

const extractPrompt = `Below is a speech-to-text transcript of the first seconds of an audiobook.
Audiobooks usually open by announcing the title, the author and the narrator, sometimes the series and the book's number in it.
Extract what is actually said. Do not guess anything that is not in the transcript; leave such fields empty.
Reply with JSON only, in this shape:
{"title": "", "author": "", "narrator": "", "series": "", "series_index": ""}

Transcript:
"""%s"""`

// ExtractGuess asks the LLM for title/author/narrator. With no LLM configured
// it falls back to simple pattern matching.
func ExtractGuess(ctx context.Context, cfg Config, transcript string) (Guess, error) {
	if cfg.OllamaURL == "" {
		return heuristicGuess(transcript), nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	reqBody := mustJSON(map[string]any{
		"model": cfg.OllamaModel, "prompt": fmt.Sprintf(extractPrompt, transcript),
		"format": "json", "stream": false, "options": map[string]any{"temperature": 0},
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(cfg.OllamaURL, "/")+"/api/generate", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Guess{}, serviceDown("LLM is not reachable at %s: %v", cfg.OllamaURL, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == 404 {
		return Guess{}, serviceDown("LLM model %s is not available yet (still downloading?)", cfg.OllamaModel)
	}
	if resp.StatusCode != 200 {
		return Guess{}, serviceDown("LLM returned %s: %.200s", resp.Status, raw)
	}
	var out struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Guess{}, fmt.Errorf("unexpected LLM reply: %w", err)
	}
	var g Guess
	if err := json.Unmarshal([]byte(out.Response), &g); err != nil {
		// Some models return series_index as a number.
		var loose map[string]any
		if json.Unmarshal([]byte(out.Response), &loose) != nil {
			return heuristicGuess(transcript), nil
		}
		str := func(k string) string {
			if v, ok := loose[k]; ok && v != nil {
				return strings.TrimSpace(fmt.Sprint(v))
			}
			return ""
		}
		g = Guess{Title: str("title"), Author: str("author"), Narrator: str("narrator"), Series: str("series"), SeriesIndex: str("series_index")}
	}
	g.Method = "llm"
	return g, nil
}

const personName = `(\p{Lu}[\p{L}'-]+(?:\s+(?:\p{Lu}\.|\p{Lu}[\p{L}'-]+)){0,3})`

var (
	byRe           = regexp.MustCompile(`(?i:,?\s+(?:written\s+)?by\s+)` + personName)
	narratedRe     = regexp.MustCompile(`(?i:(?:narrated|read|performed)\s+by\s+)` + personName)
	bookNRe        = regexp.MustCompile(`(?i)\bbook\s+(\d+|one|two|three|four|five|six|seven|eight|nine|ten)\s+(?:of|in)\s+(?:the\s+)?(.{2,60}?)(?:[,.]|$)`)
	sentenceRe     = regexp.MustCompile(`[.!?]+\s+`)
	narratorLeadRe = regexp.MustCompile(`(?i)(narrated|read|performed)$`)
	introRe        = regexp.MustCompile(`(?i)^(this is audible|audible (studios )?presents|.*\bpresents)$`)
)

var wordNums = map[string]string{"one": "1", "two": "2", "three": "3", "four": "4", "five": "5", "six": "6", "seven": "7", "eight": "8", "nine": "9", "ten": "10"}

// heuristicGuess reads the usual audiobook opening ("Title, by Author.
// Narrated by Narrator.") without an LLM.
func heuristicGuess(t string) Guess {
	g := Guess{Method: "heuristic"}
	for _, m := range byRe.FindAllStringSubmatchIndex(t, -1) {
		before := t[:m[0]]
		if narratorLeadRe.MatchString(strings.TrimSpace(before)) {
			continue // that "by" names the narrator
		}
		g.Author = t[m[2]:m[3]]
		var kept []string
		for _, sent := range sentenceRe.Split(before, -1) {
			sent = strings.Trim(sent, " ,.\"'")
			if sent == "" || introRe.MatchString(sent) || bookNRe.MatchString(sent) {
				continue
			}
			kept = append(kept, sent)
		}
		if len(kept) > 0 {
			g.Title = kept[len(kept)-1]
		}
		break
	}
	if m := narratedRe.FindStringSubmatch(t); m != nil {
		g.Narrator = m[1]
	}
	if m := bookNRe.FindStringSubmatch(t); m != nil {
		g.SeriesIndex, g.Series = m[1], strings.TrimSpace(m[2])
		if n, ok := wordNums[strings.ToLower(m[1])]; ok {
			g.SeriesIndex = n
		}
	}
	return g
}

// pingService reports whether something answers HTTP at base+path.
func pingService(ctx context.Context, base, path string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(base, "/")+path, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("returned %s", resp.Status)
	}
	return nil
}

// ollamaHasModel checks the model is pulled, and starts the pull if not.
func ollamaHasModel(ctx context.Context, cfg Config) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(cfg.OllamaURL, "/")+"/api/tags", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	json.NewDecoder(resp.Body).Decode(&tags)
	want := cfg.OllamaModel
	if !strings.Contains(want, ":") {
		want += ":latest"
	}
	for _, m := range tags.Models {
		if m.Name == want || m.Name == cfg.OllamaModel {
			return true, nil
		}
	}
	return false, nil
}

func ollamaPull(cfg Config) error {
	body := mustJSON(map[string]any{"model": cfg.OllamaModel, "name": cfg.OllamaModel, "stream": false})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(cfg.OllamaURL, "/")+"/api/pull", strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != 200 {
		return fmt.Errorf("pull returned %s: %s", resp.Status, raw)
	}
	return nil
}
