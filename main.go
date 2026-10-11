// Command aural organizes an audiobook library into Author/Series/Title
// folders, with a preview before anything moves.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "address for the web UI")
	source := flag.String("source", "", "audiobook library folder")
	dest := flag.String("dest", "", "folder to organize into (default: the library folder)")
	copyOnly := flag.Bool("copy", false, "copy instead of move")
	planOnly := flag.Bool("plan", false, "print the plan for -source and exit without changing anything")
	flag.Parse()
	if *dest == "" {
		*dest = *source
	}

	if *planOnly {
		if err := printPlan(*source, *dest); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	app := NewApp(*source, *dest, *copyOnly)
	if *source != "" {
		app.rescan()
	}
	log.Printf("Aural Organizer at http://%s", *addr)
	log.Fatal(http.ListenAndServe(*addr, app.Routes()))
}

func printPlan(source, dest string) error {
	s, err := Scan(source)
	if err != nil {
		return err
	}
	if err := s.CheckCounts(); err != nil {
		return err
	}
	p := BuildPlan(s, dest)
	for _, bp := range p.Books {
		status := "move "
		switch {
		case len(bp.Problems) > 0:
			status = "HELD "
		case bp.InPlace:
			status = "ok   "
		}
		to := "?"
		if bp.TargetDir != "" {
			to, _ = filepath.Rel(p.Dest, bp.TargetDir)
		}
		fmt.Printf("%s %-50s -> %s\n", status, bp.Book.SourceRel, to)
		for _, pr := range bp.Problems {
			fmt.Printf("        held: %s\n", pr)
		}
		for _, w := range bp.Book.Warnings {
			fmt.Printf("        check: %s\n", w)
		}
	}
	fmt.Printf("\n%d audio files in %d books: %d books to move (%d files), %d already in place, %d held (%d audio files). Leftover non-audio: %d\n",
		s.AudioFiles, len(s.Books), p.BooksReady, p.FilesReady, p.BooksInPlace, p.BooksHeld, p.AudioHeld, len(s.Leftover))
	return nil
}
