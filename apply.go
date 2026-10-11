package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type ApplyResult struct {
	Journal      string
	Moved        int
	Copied       int
	Failed       []string
	BooksDone    int
	BooksPartial int
}

type journalEntry struct {
	Op   string `json:"op"` // "move" or "copy"
	From string `json:"from"`
	To   string `json:"to"`
}

// Apply carries out the ready books in the plan. It never overwrites a file,
// records every completed operation in a journal so it can be undone, and
// with copy=true leaves the originals untouched.
func Apply(p *Plan, sourceRoot string, copyOnly bool) (*ApplyResult, error) {
	if err := os.MkdirAll(p.Dest, 0o755); err != nil {
		return nil, err
	}
	res := &ApplyResult{Journal: filepath.Join(p.Dest, ".aural-journal-"+time.Now().Format("20060102-150405")+".jsonl")}
	jf, err := os.OpenFile(res.Journal, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("cannot write journal: %w", err)
	}
	defer jf.Close()
	enc := json.NewEncoder(jf)

	emptied := map[string]bool{}
	for _, bp := range p.Books {
		if !bp.Ready() {
			continue
		}
		ok := 0
		for _, m := range bp.Moves {
			op, err := transfer(m.From, m.To, copyOnly)
			if err != nil {
				res.Failed = append(res.Failed, fmt.Sprintf("%s: %v", m.From, err))
				continue
			}
			if err := enc.Encode(journalEntry{Op: op, From: m.From, To: m.To}); err != nil {
				return res, fmt.Errorf("journal write failed, stopping: %w", err)
			}
			jf.Sync()
			if op == "copy" {
				res.Copied++
			} else {
				res.Moved++
				emptied[filepath.Dir(m.From)] = true
			}
			ok++
		}
		if ok == len(bp.Moves) {
			res.BooksDone++
		} else {
			res.BooksPartial++
		}
	}
	removeEmptyDirs(emptied, sourceRoot)
	return res, nil
}

// transfer renames, falling back to copy+verify+delete across disks.
func transfer(from, to string, copyOnly bool) (string, error) {
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return "", err
	}
	if st, err := os.Lstat(to); err == nil && !sameFile(st, from) {
		return "", errors.New("target already exists")
	}
	if !copyOnly {
		if err := os.Rename(from, to); err == nil {
			return "move", nil
		}
		// Most likely a different disk; fall through to copy then delete.
	}
	if err := copyVerified(from, to); err != nil {
		return "", err
	}
	if copyOnly {
		return "copy", nil
	}
	if err := os.Remove(from); err != nil {
		return "copy", fmt.Errorf("copied but could not remove original: %w", err)
	}
	return "move", nil
}

func copyVerified(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	tmp := to + ".aural-partial"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != st.Size() {
		err = fmt.Errorf("copied %d of %d bytes", n, st.Size())
	}
	if err == nil {
		if _, e := os.Lstat(to); e == nil {
			err = errors.New("target appeared during copy")
		} else {
			err = os.Rename(tmp, to)
		}
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	os.Chtimes(to, st.ModTime(), st.ModTime())
	return nil
}

// removeEmptyDirs deletes now-empty source folders, walking up but never
// above (or including) root. Folders with anything left in them stay.
func removeEmptyDirs(dirs map[string]bool, root string) {
	root = filepath.Clean(root)
	list := make([]string, 0, len(dirs))
	for d := range dirs {
		list = append(list, d)
	}
	sort.Slice(list, func(i, j int) bool { return len(list[i]) > len(list[j]) })
	for _, d := range list {
		for d != root && strings.HasPrefix(d, root+string(filepath.Separator)) {
			if os.Remove(d) != nil { // fails unless empty
				break
			}
			d = filepath.Dir(d)
		}
	}
}

// Undo reverses a journal, newest operation first. Moves go back; copies are deleted.
func Undo(journal string) (restored int, failed []string, err error) {
	f, err := os.Open(journal)
	if err != nil {
		return 0, nil, err
	}
	var entries []journalEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e journalEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			entries = append(entries, e)
		}
	}
	f.Close()
	emptied := map[string]bool{}
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		var err error
		if e.Op == "copy" {
			err = os.Remove(e.To)
		} else {
			_, err = transfer(e.To, e.From, false)
		}
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", e.To, err))
			continue
		}
		emptied[filepath.Dir(e.To)] = true
		restored++
	}
	removeEmptyDirs(emptied, filepath.Dir(journal))
	if len(failed) == 0 {
		os.Rename(journal, journal+".undone")
	}
	return restored, failed, nil
}

// Journals lists journals in dest that have not been undone, newest first.
func Journals(dest string) []string {
	m, _ := filepath.Glob(filepath.Join(dest, ".aural-journal-*.jsonl"))
	sort.Sort(sort.Reverse(sort.StringSlice(m)))
	return m
}
