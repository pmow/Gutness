package main

import (
	"fmt"
	"strings"
	"testing"
)

func seq(n int, f string) []string {
	var out []string
	for i := 1; i <= n; i++ {
		out = append(out, fmt.Sprintf(f, i, n))
	}
	return out
}

func TestDetectSet(t *testing.T) {
	cases := []struct {
		name  string
		files []string
		kind  string
		first string
	}{
		{"of style", seq(53, "Part %02d of %d.mp3"), KindSet, "Part 01 of 53.mp3"},
		{"dash style, 3 digits", seq(12, "%03d-%d.mp3"), KindSet, "001-12.mp3"},
		{"total first", []string{"12 - 01.mp3", "12 - 02.mp3", "12 - 12.mp3"}, KindNotASet, ""},
		{"total first ok", seq(3, "x_%[2]d_%[1]d.mp3"), KindSet, "x_3_1.mp3"},
		{"extra numbers", seq(10, "Book 2 (2019) %02d of %d 64kbps.mp3"), KindSet, "Book 2 (2019) 01 of 10 64kbps.mp3"},
		{"last part equals total", seq(2, "%d of %d.mp3"), KindSet, "1 of 2.mp3"},
		{"no total in names", []string{"01.mp3", "02.mp3", "03.mp3"}, KindNotASet, ""},
		{"missing part", append(seq(5, "%02d of %d.mp3")[:2], seq(5, "%02d of %d.mp3")[3:]...), KindNotASet, ""},
		{"single", []string{"whatever.m4b"}, KindSingle, "whatever.m4b"},
		{"starts at 0", []string{"00 of 3.mp3", "01 of 3.mp3", "02 of 3.mp3"}, KindNotASet, ""},
	}
	for _, c := range cases {
		d := DetectSet(c.files)
		if d.Kind != c.kind || (c.first != "" && d.First() != c.first) {
			t.Errorf("%s: got %s first=%q reason=%q", c.name, d.Kind, d.First(), d.Reason)
		}
	}
}

func TestDetectSetMissingReason(t *testing.T) {
	files := seq(5, "%02d of %d.mp3")
	files = append(files[:2], files[3:]...)
	d := DetectSet(files)
	if !strings.Contains(d.Reason, "missing part 3") {
		t.Errorf("reason = %q", d.Reason)
	}
}

func TestDetectSetOrder(t *testing.T) {
	d := DetectSet(seq(12, "Track %d of %d.mp3")) // unpadded sorts 1,10,11,12,2...
	for i, name := range d.Ordered {
		if want := fmt.Sprintf("Track %d of 12.mp3", i+1); name != want {
			t.Fatalf("position %d = %q, want %q", i, name, want)
		}
	}
}
