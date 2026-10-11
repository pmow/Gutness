package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/dhowden/tag"
)

type tags struct {
	title, album, artist, albumArtist, composer, narrator, series, seriesIndex, year string
}

// readTags returns whatever embedded tags it can read; unreadable files give empty tags.
func readTags(path string) tags {
	f, err := os.Open(path)
	if err != nil {
		return tags{}
	}
	defer f.Close()
	m, err := tag.ReadFrom(f)
	if err != nil {
		return tags{}
	}
	t := tags{
		title:       clean(m.Title()),
		album:       clean(m.Album()),
		artist:      clean(m.Artist()),
		albumArtist: clean(m.AlbumArtist()),
		composer:    clean(m.Composer()),
	}
	if m.Year() > 0 {
		t.year = itoa(m.Year())
	}
	for k, v := range m.Raw() {
		// MP4 atom names use a raw 0xA9 byte where people write ©.
		key, val := strings.ToLower(strings.ReplaceAll(k, "\xa9", "©")), ""
		switch x := v.(type) {
		case string:
			val = x
		case *tag.Comm: // ID3 TXXX: the real key is the description
			key, val = strings.ToLower(x.Description), x.Text
		case int:
			val = itoa(x)
		default:
			continue
		}
		val = clean(val)
		if val == "" {
			continue
		}
		switch key {
		case "series", "mvnm", "©mvn", "grp1", "©grp":
			if t.series == "" || key == "series" {
				t.series = val
			}
		case "series-part", "series_part", "seriespart", "series_index", "mvin", "©mvi":
			t.seriesIndex = val
		case "narrator", "narratedby", "©nrt", "tcom":
			t.narrator = val
		}
	}
	if t.narrator == "" {
		t.narrator = t.composer
	}
	return t
}

var (
	spaceRe     = regexp.MustCompile(`\s+`)
	junkRe      = regexp.MustCompile(`(?i)\s*[\(\[](unabridged|abridged|audiobook|retail|m4b|mp3)[\)\]]`)
	leadIndexRe = regexp.MustCompile(`(?i)^(?:book|vol(?:ume)?\.?|#)?\s*(\d{1,3}(?:\.\d)?)\s*[-._:)]\s*(.+)$`)
	bookNumRe   = regexp.MustCompile(`(?i)[,(\s-]+(?:book|vol(?:ume)?\.?|#)\s*(\d{1,3}(?:\.\d)?)\)?\s*$`)
	trackNameRe = regexp.MustCompile(`(?i)^(track|part|pt|chapter|ch|disc|cd)?[\s._-]*\d+$`)
	yearRe      = regexp.MustCompile(`\s*[\(\[]((?:19|20)\d\d)[\)\]]\s*`)
)

func clean(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	return strings.TrimSpace(spaceRe.ReplaceAllString(s, " "))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// folderGuess derives author/series/title from the path below the scan root.
func folderGuess(b *Book) (author, series, index, title, year string) {
	parts := strings.Split(filepath.ToSlash(b.SourceRel), "/")
	if b.SourceRel == "." {
		parts = nil
	}
	// Use the file name when the file is the book: several books share a
	// folder, the file sits at the root, or it is "Author/Title.m4b".
	if b.AudioCount == 1 {
		stem := strings.TrimSuffix(filepath.Base(audioFile(b).Abs), filepath.Ext(audioFile(b).Abs))
		folderIsBook := len(parts) == 1 && (strings.Contains(parts[0], " - ") || strings.EqualFold(clean(stem), clean(parts[0])))
		if b.Standalone || len(parts) == 0 || (len(parts) == 1 && !folderIsBook && !trackNameRe.MatchString(stem)) {
			parts = append(parts, stem)
		}
	}
	if len(parts) == 0 {
		return // loose files directly in the root: nothing to infer
	}
	last := junkRe.ReplaceAllString(parts[len(parts)-1], "")
	if m := yearRe.FindStringSubmatch(last); m != nil {
		year = m[1]
		last = clean(yearRe.ReplaceAllString(last, " "))
	}
	// "Author - Series 03 - Title" or "Author - Title" in one name
	if segs := strings.Split(last, " - "); len(parts) == 1 && len(segs) >= 2 {
		author = clean(segs[0])
		if len(segs) >= 3 {
			series = clean(segs[1])
			last = strings.Join(segs[2:], " - ")
		} else {
			last = segs[1]
		}
	}
	if m := leadIndexRe.FindStringSubmatch(last); m != nil {
		index, last = m[1], m[2]
	} else if m := bookNumRe.FindStringSubmatch(last); m != nil {
		index, last = m[1], bookNumRe.ReplaceAllString(last, "")
	}
	if m := regexp.MustCompile(`^(.*\S)\s+(\d{1,3})$`).FindStringSubmatch(series); m != nil && index == "" {
		series, index = m[1], m[2]
	}
	title = clean(last)
	if len(parts) >= 2 && author == "" {
		author = clean(parts[0])
	}
	if len(parts) >= 3 && series == "" {
		series = clean(parts[len(parts)-2])
	}
	return
}

func fillMetadata(b *Book) {
	var first tags
	albums := map[string]bool{}
	for _, f := range b.Files {
		if !f.Audio {
			continue
		}
		t := readTags(f.Abs)
		if first == (tags{}) {
			first = t
		}
		if t.album != "" {
			albums[strings.ToLower(t.album)] = true
		}
		if len(albums) > 1 || len(b.Files) > 60 {
			break // enough to know; don't read every tag of huge books
		}
	}
	if len(albums) > 1 {
		b.Warnings = append(b.Warnings, "files in this folder have different album tags; check it is one book")
	}
	set := func(field *string, name, val, from string) {
		if *field == "" && val != "" {
			*field, b.From[name] = val, from
		}
	}
	title := junkRe.ReplaceAllString(first.album, "")
	if title == "" && b.AudioCount == 1 {
		title = junkRe.ReplaceAllString(first.title, "")
	}
	author := first.albumArtist
	if author == "" {
		author = first.artist
	}
	set(&b.Author, "author", clean(author), "tags")
	set(&b.Title, "title", clean(title), "tags")
	set(&b.Series, "series", first.series, "tags")
	set(&b.SeriesIndex, "index", first.seriesIndex, "tags")
	set(&b.Narrator, "narrator", first.narrator, "tags")
	set(&b.Year, "year", first.year, "tags")

	fa, fs, fi, ft, fy := folderGuess(b)
	set(&b.Author, "author", fa, "folder")
	set(&b.Title, "title", ft, "folder")
	set(&b.Series, "series", fs, "folder")
	set(&b.SeriesIndex, "index", fi, "folder")
	set(&b.Year, "year", fy, "folder")
	if b.From["author"] == "tags" && fa != "" && !b.Standalone && nameKey(fa) != nameKey(b.Author) {
		b.Warnings = append(b.Warnings, "tag author \""+b.Author+"\" differs from folder \""+fa+"\"")
	}
}

func audioFile(b *Book) File {
	for _, f := range b.Files {
		if f.Audio {
			return f
		}
	}
	return File{}
}

// nameKey compares names ignoring order, case and punctuation,
// so "Le Guin, Ursula K." matches "Ursula K. Le Guin".
func nameKey(s string) string {
	words := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r > 127)
	})
	sort.Strings(words)
	return strings.Join(words, " ")
}
