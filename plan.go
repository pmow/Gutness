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
}

// OrgPlan is the final, renaming step for one recognized item.
type OrgPlan struct {
	Item      *Item
	Root      string // library the item belongs to
	TargetDir string
	Moves     []Move
	Problems  []string // any problem holds the item back
	InPlace   bool
}

func (p OrgPlan) Ready() bool { return len(p.Problems) == 0 && !p.InPlace && len(p.Moves) > 0 }

var badChars = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)

// safeName makes a string usable as one folder name on Windows, macOS and Linux.
func safeName(s string) string {
	s = badChars.ReplaceAllString(s, "-")
	s = strings.Join(strings.Fields(s), " ")
	s = strings.Trim(s, " .")
	if r := []rune(s); len(r) > 120 {
		s = strings.TrimRight(string(r[:120]), " .")
	}
	return s
}

// TargetDir follows Audiobookshelf's folder convention:
// Author/Series/Vol 1 - Title, or Author/Title without a series.
func TargetDir(root string, it *Item) string {
	title := safeName(it.Title)
	if it.Series != "" && it.SeriesIndex != "" {
		title = safeName("Vol " + absIndex(it.SeriesIndex) + " - " + it.Title)
	}
	parts := []string{root, safeName(it.Author)}
	if s := safeName(it.Series); s != "" {
		parts = append(parts, s)
	}
	return filepath.Join(append(parts, title)...)
}

// absIndex drops leading zeros ("03" -> "3", "01.5" -> "1.5"), the form
// Audiobookshelf reads back as the series sequence.
func absIndex(s string) string {
	t := strings.TrimLeft(strings.TrimSpace(s), "0")
	if t == "" || t[0] == '.' {
		t = "0" + t
	}
	return t
}

// PlanOrganize works out where each item's folder contents would go. Every
// file directly inside the item's folder moves (audio plus covers etc.);
// subfolders stay put.
func PlanOrganize(items []*Item, roots map[int64]string) []OrgPlan {
	var plans []OrgPlan
	claimed := map[string][]int{}
	for _, it := range items {
		p := OrgPlan{Item: it, Root: roots[it.LibraryID]}
		if safeName(it.Author) == "" {
			p.Problems = append(p.Problems, "no author")
		}
		if safeName(it.Title) == "" {
			p.Problems = append(p.Problems, "no title")
		}
		if it.Missing {
			p.Problems = append(p.Problems, "folder is gone; rescan the library")
		}
		if len(p.Problems) == 0 {
			p.TargetDir = TargetDir(p.Root, it)
			if p.TargetDir == filepath.Clean(it.Folder) {
				p.InPlace = true
			} else if rel, err := filepath.Rel(it.Folder, p.TargetDir); err == nil && !strings.HasPrefix(rel, "..") {
				p.Problems = append(p.Problems, "target would be inside the current folder")
			} else {
				entries, err := os.ReadDir(it.Folder)
				if err != nil {
					p.Problems = append(p.Problems, err.Error())
				}
				for _, e := range entries {
					if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), ".") {
						continue
					}
					info, _ := e.Info()
					to := filepath.Join(p.TargetDir, e.Name())
					if st, err := os.Lstat(to); err == nil && !sameFile(st, filepath.Join(it.Folder, e.Name())) {
						p.Problems = append(p.Problems, "a file already exists at "+to)
						break
					}
					var size int64
					if info != nil {
						size = info.Size()
					}
					p.Moves = append(p.Moves, Move{From: filepath.Join(it.Folder, e.Name()), To: to, Bytes: size})
				}
			}
			key := strings.ToLower(p.TargetDir)
			claimed[key] = append(claimed[key], len(plans))
		}
		plans = append(plans, p)
	}
	for _, idx := range claimed {
		if len(idx) > 1 {
			for _, i := range idx {
				plans[i].Problems = append(plans[i].Problems, "another book would go to the same folder")
			}
		}
	}
	return plans
}

// sameFile catches case-only renames on case-insensitive disks.
func sameFile(st os.FileInfo, path string) bool {
	st2, err := os.Lstat(path)
	return err == nil && os.SameFile(st, st2)
}
