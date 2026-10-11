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
	Journal string
	Moved   int
	Failed  []string
	Items   []int64 // items whose files all moved
}

type journalEntry struct {
	Op     string `json:"op"` // "move"
	From   string `json:"from"`
	To     string `json:"to"`
	Item   int64  `json:"item,omitempty"`
	Folder string `json:"folder,omitempty"` // the item's folder before the move
	Root   string `json:"root,omitempty"`   // library root; empty folders are pruned up to here
}

// Apply moves the files of every ready plan. It never overwrites a file and
// records each completed move in a journal under dataDir so it can be undone.
func Apply(plans []OrgPlan, dataDir string) (*ApplyResult, error) {
	dir := filepath.Join(dataDir, "journals")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	res := &ApplyResult{Journal: filepath.Join(dir, time.Now().Format("20060102-150405")+".jsonl")}
	jf, err := os.OpenFile(res.Journal, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("cannot write journal: %w", err)
	}
	defer jf.Close()
	enc := json.NewEncoder(jf)
	for _, p := range plans {
		if !p.Ready() {
			continue
		}
		ok := 0
		for _, m := range p.Moves {
			if _, err := transfer(m.From, m.To, false); err != nil {
				res.Failed = append(res.Failed, fmt.Sprintf("%s: %v", m.From, err))
				continue
			}
			if err := enc.Encode(journalEntry{Op: "move", From: m.From, To: m.To, Item: p.Item.ID, Folder: p.Item.Folder, Root: p.Root}); err != nil {
				return res, fmt.Errorf("journal write failed, stopping: %w", err)
			}
			jf.Sync()
			res.Moved++
			ok++
		}
		if ok == len(p.Moves) {
			res.Items = append(res.Items, p.Item.ID)
		}
		removeEmptyDirs(map[string]bool{p.Item.Folder: true}, p.Root)
	}
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

// Undo reverses a journal, newest move first, and returns the entries it
// restored so the caller can point items back at their old folders.
func Undo(journal string) (restored []journalEntry, failed []string, err error) {
	f, err := os.Open(journal)
	if err != nil {
		return nil, nil, err
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
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if _, err := transfer(e.To, e.From, false); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", e.To, err))
			continue
		}
		removeEmptyDirs(map[string]bool{filepath.Dir(e.To): true}, e.Root)
		restored = append(restored, e)
	}
	if len(failed) == 0 {
		os.Rename(journal, journal+".undone")
	}
	return restored, failed, nil
}

// Journals lists journals that have not been undone, newest first.
func Journals(dataDir string) []string {
	m, _ := filepath.Glob(filepath.Join(dataDir, "journals", "*.jsonl"))
	sort.Sort(sort.Reverse(sort.StringSlice(m)))
	return m
}
