package main

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func touch(t *testing.T, root string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func listFiles(t *testing.T, root string) []string {
	var out []string
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Ext(p) != ".jsonl" && filepath.Ext(p) != ".undone" {
			r, _ := filepath.Rel(root, p)
			out = append(out, filepath.ToSlash(r))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func TestFolderGuess(t *testing.T) {
	cases := []struct {
		rel, file                          string
		author, series, index, title, year string
	}{
		{"Terry Pratchett/Discworld/03 - Equal Rites", "01.mp3", "Terry Pratchett", "Discworld", "03", "Equal Rites", ""},
		{"Terry Pratchett/Discworld/Vol 3 - Equal Rites", "01.mp3", "Terry Pratchett", "Discworld", "3", "Equal Rites", ""},
		{"Andy Weir - Project Hail Mary (2021)", "a.mp3", "Andy Weir", "", "", "Project Hail Mary", "2021"},
		{"James Corey - The Expanse 01 - Leviathan Wakes", "a.mp3", "James Corey", "The Expanse", "01", "Leviathan Wakes", ""},
		{"Brandon Sanderson/Mistborn/The Final Empire, Book 1", "a.mp3", "Brandon Sanderson", "Mistborn", "1", "The Final Empire", ""},
		{"Ursula K. Le Guin", "A Wizard of Earthsea (Unabridged).m4b", "Ursula K. Le Guin", "", "", "A Wizard of Earthsea", ""},
	}
	for _, c := range cases {
		b := &Book{SourceRel: c.rel, Files: []File{{Abs: "/x/" + c.file, Audio: true}, {Abs: "/x/b.mp3", Audio: true}}, AudioCount: 2}
		if filepath.Ext(c.file) == ".m4b" {
			b.Files, b.AudioCount = b.Files[:1], 1
		}
		a, s, i, ti, y := folderGuess(b)
		if a != c.author || s != c.series || i != c.index || ti != c.title || y != c.year {
			t.Errorf("%s: got %q/%q/%q/%q/%q", c.rel, a, s, i, ti, y)
		}
	}
}

func TestScanCountsEveryFile(t *testing.T) {
	root := t.TempDir()
	touch(t, root,
		"Author A/Book One/01.mp3", "Author A/Book One/02.mp3", "Author A/Book One/cover.jpg",
		"Author B/Series/02 - Book Two/CD1/01.mp3", "Author B/Series/02 - Book Two/Disc 2/01.mp3",
		"loose1.mp3", "loose2.m4b", "notes.txt", ".hidden/skip.mp3",
	)
	s, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if s.AudioFiles != 6 {
		t.Fatalf("AudioFiles = %d, want 6", s.AudioFiles)
	}
	if err := s.CheckCounts(); err != nil {
		t.Fatal(err)
	}
	if len(s.Books) != 4 { // Book One, Book Two (two discs merged), two loose files
		for _, b := range s.Books {
			t.Log(b.SourceRel, b.Title, len(b.Files))
		}
		t.Fatalf("got %d books, want 4", len(s.Books))
	}
	if len(s.Leftover) != 1 || filepath.Base(s.Leftover[0]) != "notes.txt" {
		t.Fatalf("leftover = %v", s.Leftover)
	}
}

func TestApplyAndUndo(t *testing.T) {
	src, dest := t.TempDir(), t.TempDir()
	touch(t, src,
		"Andy Weir - Project Hail Mary/1.mp3", "Andy Weir - Project Hail Mary/2.mp3", "Andy Weir - Project Hail Mary/cover.jpg",
		"Terry Pratchett/Discworld/03 - Equal Rites/CD1/01.mp3",
	)
	before := listFiles(t, src)
	s, _ := Scan(src)
	p := BuildPlan(s, dest)
	if p.BooksReady != 2 || p.FilesReady != 4 {
		t.Fatalf("ready books=%d files=%d", p.BooksReady, p.FilesReady)
	}
	res, err := Apply(p, src, false)
	if err != nil || len(res.Failed) > 0 {
		t.Fatal(err, res.Failed)
	}
	want := []string{
		"Andy Weir/Project Hail Mary/1.mp3", "Andy Weir/Project Hail Mary/2.mp3", "Andy Weir/Project Hail Mary/cover.jpg",
		"Terry Pratchett/Discworld/Vol 3 - Equal Rites/CD1/01.mp3",
	}
	if got := listFiles(t, dest); !equal(got, want) {
		t.Fatalf("dest = %v", got)
	}
	if got := listFiles(t, src); len(got) != 0 {
		t.Fatalf("source not emptied: %v", got)
	}
	if n, failed, err := Undo(res.Journal); err != nil || n != 4 || len(failed) > 0 {
		t.Fatalf("undo: %d %v %v", n, failed, err)
	}
	if got := listFiles(t, src); !equal(got, before) {
		t.Fatalf("after undo = %v, want %v", got, before)
	}
}

func TestNeverOverwrites(t *testing.T) {
	src, dest := t.TempDir(), t.TempDir()
	touch(t, src, "Andy Weir - Project Hail Mary/1.mp3")
	touch(t, dest, "Andy Weir/Project Hail Mary/1.mp3")
	s, _ := Scan(src)
	p := BuildPlan(s, dest)
	if p.BooksHeld != 1 || p.BooksReady != 0 {
		t.Fatalf("held=%d ready=%d", p.BooksHeld, p.BooksReady)
	}
	if _, err := transfer(filepath.Join(src, "Andy Weir - Project Hail Mary/1.mp3"), filepath.Join(dest, "Andy Weir/Project Hail Mary/1.mp3"), false); err == nil {
		t.Fatal("transfer overwrote an existing file")
	}
}

func TestDuplicateTargetsHeld(t *testing.T) {
	src := t.TempDir()
	touch(t, src, "Andy Weir - Project Hail Mary/1.mp3", "dupes/Andy Weir - Project Hail Mary/1.mp3")
	s, _ := Scan(src)
	for _, b := range s.Books {
		b.Author, b.Title = "Andy Weir", "Project Hail Mary"
	}
	p := BuildPlan(s, t.TempDir())
	if p.BooksHeld != 2 {
		t.Fatalf("held=%d, want both books held", p.BooksHeld)
	}
}

func TestSafeName(t *testing.T) {
	if got := safeName(`AC/DC: "Live"?  `); got != "AC-DC- -Live--" {
		t.Errorf("got %q", got)
	}
	for in, want := range map[string]string{"03": "3", "1": "1", "01.5": "1.5", "0": "0", "10": "10"} {
		if got := absIndex(in); got != want {
			t.Errorf("absIndex(%q) = %q, want %q", in, got, want)
		}
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
