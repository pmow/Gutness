package main

import (
	"sort"
	"strings"
	"unicode"
)

var stopwords = map[string]bool{"the": true, "a": true, "an": true, "of": true, "and": true, "by": true, "in": true, "to": true}

func tokens(s string) []string {
	f := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	out := f[:0]
	for _, w := range f {
		if !stopwords[w] {
			out = append(out, w)
		}
	}
	return out
}

// dice is the overlap of two word sets, 0..1.
func dice(a, b string) float64 {
	ta, tb := tokens(a), tokens(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	set := map[string]int{}
	for _, w := range ta {
		set[w]++
	}
	common := 0
	for _, w := range tb {
		if set[w] > 0 {
			set[w]--
			common++
		}
	}
	return 2 * float64(common) / float64(len(ta)+len(tb))
}

// containment is the share of s's words that occur in the transcript.
func containment(s string, transcript map[string]bool) float64 {
	t := tokens(s)
	if len(t) == 0 {
		return 0
	}
	n := 0
	for _, w := range t {
		if transcript[w] {
			n++
		}
	}
	return float64(n) / float64(len(t))
}

// ScoreCandidates rates each candidate against what was heard, best first.
// Title counts 65%, author 35%. When the guess lacks a field, the raw
// transcript is searched for the candidate's words instead.
func ScoreCandidates(g Guess, transcript string, cs []Candidate) []Candidate {
	heard := map[string]bool{}
	for _, w := range tokens(transcript) {
		heard[w] = true
	}
	for i := range cs {
		c := &cs[i]
		var title float64
		if g.Title != "" {
			title = max(dice(g.Title, c.Title), dice(g.Title, c.Title+" "+c.Subtitle))
		} else {
			title = containment(c.Title, heard)
		}
		var author float64
		for _, a := range c.Authors {
			if g.Author != "" {
				author = max(author, dice(g.Author, a))
			} else {
				author = max(author, containment(a, heard))
			}
		}
		c.Score = 0.65*title + 0.35*author
		if c.API == "audible" && c.Score > 0.5 {
			c.Score = min(1, c.Score+0.03) // carries narrator and series details
		}
	}
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].Score > cs[j].Score })
	return cs
}

const (
	matchThreshold  = 0.8
	reviewThreshold = 0.45
)

// nameKey compares titles ignoring case, punctuation and small words.
func nameKey(s string) string { return strings.Join(tokens(s), " ") }
