package main

import (
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type ScanSummary struct {
	Folders, AudioFiles, Sets, SetFiles, Singles, NotSets, NotSetFiles, New, Changed, Missing int
	Errors                                                                                    []string
}

func (s ScanSummary) String() string {
	out := fmt.Sprintf("%d audio files in %d folders: %d sets (%d files), %d single files, %d folders not a set (%d files). %d new, %d changed, %d gone.",
		s.AudioFiles, s.Folders, s.Sets, s.SetFiles, s.Singles, s.NotSets, s.NotSetFiles, s.New, s.Changed, s.Missing)
	if len(s.Errors) > 0 {
		out += fmt.Sprintf(" %d unreadable: %s", len(s.Errors), strings.Join(s.Errors, "; "))
	}
	return out
}

func addLibrary(db *sql.DB, path string) (int64, error) {
	typed := strings.TrimSpace(path)
	if !filepath.IsAbs(typed) {
		return 0, fmt.Errorf("give the full path of the folder, starting with /")
	}
	path = filepath.Clean(typed)
	if st, err := os.Stat(path); err != nil || !st.IsDir() {
		return 0, fmt.Errorf("%s is not a readable folder (in Docker, folders outside /mnt, /media, /home and /srv need a line under volumes: in the compose file)", typed)
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO libraries(path, added) VALUES(?, ?)`, path, now()); err != nil {
		return 0, err
	}
	var id int64
	err := db.QueryRow(`SELECT id FROM libraries WHERE path=?`, path).Scan(&id)
	return id, err
}

// ScanLibrary records every folder holding audio as one item and decides
// whether it is a set. Only names and sizes are read here; no audio is decoded.
func ScanLibrary(db *sql.DB, lib Library) (ScanSummary, error) {
	var sum ScanSummary
	type folder struct {
		audio []string
		bytes int64
	}
	folders := map[string]*folder{}
	err := filepath.WalkDir(lib.Path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			sum.Errors = append(sum.Errors, err.Error())
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if p != lib.Path && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || !isAudio(d.Name()) {
			return nil
		}
		dir := filepath.Dir(p)
		f := folders[dir]
		if f == nil {
			f = &folder{}
			folders[dir] = f
		}
		f.audio = append(f.audio, d.Name())
		if info, err := d.Info(); err == nil {
			f.bytes += info.Size()
		}
		return nil
	})
	if err != nil {
		return sum, err
	}

	seen := map[string]bool{}
	for dir, f := range folders {
		seen[dir] = true
		sum.Folders++
		sum.AudioFiles += len(f.audio)
		det := DetectSet(f.audio)
		status := StPendingSample
		switch det.Kind {
		case KindSet:
			sum.Sets++
			sum.SetFiles += len(f.audio)
		case KindSingle:
			sum.Singles++
		default:
			sum.NotSets++
			sum.NotSetFiles += len(f.audio)
			status = StNotASet
		}
		files := mustJSON(det.Ordered)
		var id int64
		var oldKind, oldFiles string
		err := db.QueryRow(`SELECT id, kind, files FROM items WHERE folder=?`, dir).Scan(&id, &oldKind, &oldFiles)
		switch {
		case err == sql.ErrNoRows:
			sum.New++
			_, err = db.Exec(`INSERT INTO items(library_id, folder, kind, total, files, bytes, reason, status, created, updated)
				VALUES(?,?,?,?,?,?,?,?,?,?)`, lib.ID, dir, det.Kind, det.Total, files, f.bytes, det.Reason, status, now(), now())
		case err != nil:
		case oldKind != det.Kind || oldFiles != files:
			sum.Changed++
			// The folder's contents changed: start recognition over.
			_, err = db.Exec(`UPDATE items SET kind=?, total=?, files=?, bytes=?, reason=?, status=?, detail='', attempts=0,
				next_at=0, sample='', transcript='', guess='{}', candidates='[]', score=0, missing=0, method='', samples='[]', durations='[]', updated=? WHERE id=?`,
				det.Kind, det.Total, files, f.bytes, det.Reason, status, now(), id)
			if err == nil {
				_, err = db.Exec(`DELETE FROM jobs WHERE item_id=?`, id)
			}
		default:
			_, err = db.Exec(`UPDATE items SET missing=0, bytes=? WHERE id=?`, f.bytes, id)
		}
		if err != nil {
			return sum, err
		}
	}

	rows, err := db.Query(`SELECT id, folder FROM items WHERE library_id=? AND missing=0`, lib.ID)
	if err != nil {
		return sum, err
	}
	var gone []int64
	for rows.Next() {
		var id int64
		var dir string
		rows.Scan(&id, &dir)
		if !seen[dir] {
			gone = append(gone, id)
		}
	}
	rows.Close()
	for _, id := range gone {
		db.Exec(`UPDATE items SET missing=1, updated=? WHERE id=?`, now(), id)
	}
	sum.Missing = len(gone)
	_, err = db.Exec(`UPDATE libraries SET last_scan=?, scan_summary=? WHERE id=?`, now(), sum.String(), lib.ID)
	return sum, err
}
