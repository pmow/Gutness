package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type Move struct {
	From, To string
	Bytes    int64
	Audio    bool
}

type BookPlan struct {
	Book      *Book
	TargetDir string
	Moves     []Move
	Problems  []string // any problem means the book is left where it is
	InPlace   bool     // already organized; nothing to do
}

func (bp BookPlan) Ready() bool { return len(bp.Problems) == 0 && !bp.InPlace && !bp.Book.Skip }

type Plan struct {
	Dest  string
	Books []BookPlan
	// Counts. AudioReady + AudioInPlace + AudioHeld always equals the scan's AudioFiles.
	BooksReady, BooksInPlace, BooksHeld int
	AudioReady, AudioInPlace, AudioHeld int
	FilesReady                          int
	BytesReady                          int64
}

var badChars = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)

// safeName makes a string usable as one folder name on Windows, macOS and Linux.
func safeName(s string) string {
	s = badChars.ReplaceAllString(s, "-")
	s = clean(s)
	s = strings.Trim(s, " .")
	if r := []rune(s); len(r) > 120 {
		s = strings.TrimRight(string(r[:120]), " .")
	}
	return s
}

// TargetDir follows Audiobookshelf's folder convention:
// Author/Series/Vol 1 - Title, or Author/Title without a series.
func TargetDir(dest string, b *Book) string {
	title := safeName(b.Title)
	if b.Series != "" && b.SeriesIndex != "" {
		title = safeName("Vol " + absIndex(b.SeriesIndex) + " - " + b.Title)
	}
	parts := []string{dest, safeName(b.Author)}
	if s := safeName(b.Series); s != "" {
		parts = append(parts, s)
	}
	return filepath.Join(append(parts, title)...)
}

// absIndex drops leading zeros ("03" -> "3", "01.5" -> "1.5"), the form
// Audiobookshelf reads back as the series sequence.
func absIndex(s string) string {
	t := strings.TrimLeft(s, "0")
	if t == "" || t[0] == '.' {
		t = "0" + t
	}
	return t
}

func BuildPlan(scan *ScanResult, dest string) *Plan {
	dest, _ = filepath.Abs(dest)
	p := &Plan{Dest: dest}
	claimed := map[string][]int{} // target dir -> plan indexes
	for _, b := range scan.Books {
		bp := BookPlan{Book: b}
		if safeName(b.Author) == "" {
			bp.Problems = append(bp.Problems, "no author")
		}
		if safeName(b.Title) == "" {
			bp.Problems = append(bp.Problems, "no title")
		}
		if len(bp.Problems) == 0 {
			bp.TargetDir = TargetDir(dest, b)
			same := true
			for _, f := range b.Files {
				to := filepath.Join(bp.TargetDir, f.Rel)
				if to == f.Abs {
					continue
				}
				same = false
				bp.Moves = append(bp.Moves, Move{From: f.Abs, To: to, Bytes: f.Bytes, Audio: f.Audio})
				if st, err := os.Lstat(to); err == nil && !sameFile(st, f.Abs) {
					bp.Problems = append(bp.Problems, "a file already exists at "+to)
				}
			}
			bp.InPlace = same
			key := strings.ToLower(bp.TargetDir)
			claimed[key] = append(claimed[key], len(p.Books))
		}
		p.Books = append(p.Books, bp)
	}
	for _, idx := range claimed {
		if len(idx) > 1 {
			for _, i := range idx {
				p.Books[i].Problems = append(p.Books[i].Problems, "another book would go to the same folder")
				p.Books[i].InPlace = false
			}
		}
	}
	for _, bp := range p.Books {
		switch {
		case bp.Ready():
			p.BooksReady++
			p.AudioReady += bp.Book.AudioCount
			p.FilesReady += len(bp.Moves)
			for _, m := range bp.Moves {
				p.BytesReady += m.Bytes
			}
		case bp.InPlace && len(bp.Problems) == 0:
			p.BooksInPlace++
			p.AudioInPlace += bp.Book.AudioCount
		default:
			p.BooksHeld++
			p.AudioHeld += bp.Book.AudioCount
		}
	}
	return p
}

// sameFile catches case-only renames on case-insensitive disks.
func sameFile(st os.FileInfo, path string) bool {
	st2, err := os.Lstat(path)
	return err == nil && os.SameFile(st, st2)
}
