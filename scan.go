package main

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var audioExts = map[string]bool{
	".mp3": true, ".m4a": true, ".m4b": true, ".aac": true, ".flac": true,
	".ogg": true, ".oga": true, ".opus": true, ".wma": true, ".wav": true,
}

// Disc/part subfolders are merged into their parent book.
var discDirRe = regexp.MustCompile(`(?i)^(cd|disc|disk|part|pt)[\s._-]*\d+\b`)

type File struct {
	Abs   string // absolute source path
	Rel   string // path relative to the book's root folder (kept on move)
	Bytes int64
	Audio bool
}

type Book struct {
	ID          string
	SourceDir   string // folder the book was found in
	SourceRel   string // SourceDir relative to the scan root
	Files       []File
	AudioCount  int
	Bytes       int64
	Author      string
	Series      string
	SeriesIndex string
	Title       string
	Narrator    string
	Year        string
	From        map[string]string // field -> "tags" | "folder" | "online" | "edited"
	Warnings    []string
	Skip        bool // set by the user
	Standalone  bool // one of several single-file books sharing a folder
}

type ScanResult struct {
	Root       string
	Books      []*Book
	AudioFiles int      // every audio file found under Root
	Leftover   []string // non-audio files that will not move with any book
	Errors     []string // unreadable folders/files
}

func isAudio(name string) bool { return audioExts[strings.ToLower(filepath.Ext(name))] }

// Scan walks root and groups audio files into books. Every audio file found
// ends up in exactly one book; CheckCounts verifies that.
func Scan(root string) (*ScanResult, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("%s is not a readable folder", root)
	}
	res := &ScanResult{Root: root}

	type dirInfo struct{ audio, other []File }
	dirs := map[string]*dirInfo{}
	get := func(d string) *dirInfo {
		if dirs[d] == nil {
			dirs[d] = &dirInfo{}
		}
		return dirs[d]
	}

	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", p, err))
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if p != root && strings.HasPrefix(name, ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", p, err))
			return nil
		}
		f := File{Abs: p, Bytes: info.Size(), Audio: isAudio(name)}
		di := get(filepath.Dir(p))
		if f.Audio {
			res.AudioFiles++
			di.audio = append(di.audio, f)
		} else {
			di.other = append(di.other, f)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Fold disc folders (CD1, Disc 2, ...) into their parent.
	bookRoot := func(dir string) string {
		for dir != root && discDirRe.MatchString(filepath.Base(dir)) {
			dir = filepath.Dir(dir)
		}
		return dir
	}
	groups := map[string]*dirInfo{}
	var keys []string
	for d, di := range dirs {
		br := bookRoot(d)
		g := groups[br]
		if g == nil {
			g = &dirInfo{}
			groups[br] = g
			keys = append(keys, br)
		}
		g.audio = append(g.audio, di.audio...)
		g.other = append(g.other, di.other...)
	}
	sort.Strings(keys)

	for _, dir := range keys {
		g := groups[dir]
		rel := func(f File) File {
			f.Rel, _ = filepath.Rel(dir, f.Abs)
			return f
		}
		sort.Slice(g.audio, func(i, j int) bool { return g.audio[i].Abs < g.audio[j].Abs })
		if len(g.audio) == 0 {
			for _, f := range g.other {
				res.Leftover = append(res.Leftover, f.Abs)
			}
			continue
		}
		// A folder of several standalone .m4b files whose album tags differ is
		// several books; anything else is one book.
		// Loose files at the top of the library are each their own book.
		splits := splitStandalone(g.audio)
		if dir == root {
			splits = g.audio
		}
		if splits != nil {
			for _, f := range splits {
				f.Rel = filepath.Base(f.Abs)
				res.Books = append(res.Books, newBook(root, dir, []File{f}, true))
			}
			for _, f := range g.other {
				res.Leftover = append(res.Leftover, f.Abs)
			}
			continue
		}
		var files []File
		for _, f := range g.audio {
			files = append(files, rel(f))
		}
		for _, f := range g.other {
			files = append(files, rel(f))
		}
		res.Books = append(res.Books, newBook(root, dir, files, false))
	}
	sort.Strings(res.Leftover)
	return res, nil
}

func splitStandalone(audio []File) []File {
	if len(audio) < 2 {
		return nil
	}
	albums := map[string]bool{}
	for _, f := range audio {
		if strings.ToLower(filepath.Ext(f.Abs)) != ".m4b" || filepath.Dir(f.Abs) != filepath.Dir(audio[0].Abs) {
			return nil
		}
		t := readTags(f.Abs)
		albums[strings.ToLower(strings.TrimSpace(t.album))] = true
	}
	if len(albums) < len(audio) || albums[""] {
		return nil
	}
	return audio
}

func newBook(root, dir string, files []File, standalone bool) *Book {
	b := &Book{SourceDir: dir, Files: files, From: map[string]string{}, Standalone: standalone}
	b.SourceRel, _ = filepath.Rel(root, dir)
	for _, f := range files {
		b.Bytes += f.Bytes
		if f.Audio {
			b.AudioCount++
		}
	}
	key := dir
	if len(files) == 1 {
		key = files[0].Abs
	}
	h := sha1.Sum([]byte(key))
	b.ID = hex.EncodeToString(h[:6])
	fillMetadata(b)
	return b
}

// CheckCounts confirms every audio file found is assigned to exactly one book.
func (r *ScanResult) CheckCounts() error {
	seen := map[string]bool{}
	n := 0
	for _, b := range r.Books {
		for _, f := range b.Files {
			if !f.Audio {
				continue
			}
			if seen[f.Abs] {
				return fmt.Errorf("file assigned to two books: %s", f.Abs)
			}
			seen[f.Abs] = true
			n++
		}
	}
	if n != r.AudioFiles {
		return fmt.Errorf("found %d audio files but %d are assigned to books", r.AudioFiles, n)
	}
	return nil
}
