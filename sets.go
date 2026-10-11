package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var audioExts = map[string]bool{
	".mp3": true, ".m4a": true, ".m4b": true, ".aac": true, ".flac": true,
	".ogg": true, ".oga": true, ".opus": true, ".wma": true, ".wav": true,
}

func isAudio(name string) bool { return audioExts[strings.ToLower(filepath.Ext(name))] }

const (
	KindSet     = "set"       // files 1..N, each naming the total N
	KindSingle  = "single"    // exactly one audio file in the folder
	KindNotASet = "not_a_set" // several audio files that do not form a set
)

// Detection is the verdict for the audio files of one folder.
type Detection struct {
	Kind    string
	Total   int
	Ordered []string // audio file names, part 1 first (sets), or sorted (others)
	Reason  string   // why the folder is not a set
}

func (d Detection) First() string {
	if len(d.Ordered) == 0 {
		return ""
	}
	return d.Ordered[0]
}

var numRe = regexp.MustCompile(`\d+`)

func numbers(name string) []int {
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	var out []int
	for _, s := range numRe.FindAllString(stem, -1) {
		if len(s) > 6 {
			continue
		}
		n, _ := strconv.Atoi(s)
		out = append(out, n)
	}
	return out
}

// DetectSet applies the set rule to the audio file names found in one folder:
// the files are numbered 1..N (1, 01 or 001 style), every name also contains
// the total N, all N parts are present, and nothing else in the folder is audio.
func DetectSet(audio []string) Detection {
	names := append([]string(nil), audio...)
	sort.Strings(names)
	n := len(names)
	switch n {
	case 0:
		return Detection{Kind: KindNotASet, Reason: "no audio files"}
	case 1:
		return Detection{Kind: KindSingle, Total: 1, Ordered: names}
	}
	nums := make([][]int, n)
	for i, name := range names {
		nums[i] = numbers(name)
	}

	// The total must appear in every name. Look for a number common to all
	// names that is at least as large as the file count.
	if !allContain(nums, n) {
		if t := commonTotal(nums, n); t > n {
			return Detection{Kind: KindNotASet, Ordered: names, Total: t,
				Reason: fmt.Sprintf("names say %d parts but only %d audio files are here%s", t, n, missingList(nums, t))}
		}
		for i := range names {
			if !contains(nums[i], n) {
				return Detection{Kind: KindNotASet, Ordered: names,
					Reason: fmt.Sprintf("%d audio files, but %q does not include the total %d", n, names[i], n)}
			}
		}
	}

	// Drop one occurrence of the total from each name; the part number is the
	// remaining number at a consistent position (from the start or the end).
	rest := make([][]int, n)
	for i := range nums {
		rest[i] = removeOnce(nums[i], n)
	}
	for _, fromEnd := range []bool{false, true} {
		for p := 0; ; p++ {
			order, ok, inRange := partOrder(rest, p, fromEnd, n)
			if !inRange {
				break
			}
			if ok {
				ordered := make([]string, n)
				for i, part := range order {
					ordered[part-1] = names[i]
				}
				return Detection{Kind: KindSet, Total: n, Ordered: ordered}
			}
		}
	}
	return Detection{Kind: KindNotASet, Ordered: names,
		Reason: fmt.Sprintf("%d audio files name the total %d, but their part numbers do not run 1 to %d", n, n, n)}
}

// partOrder reads position p of every file's numbers and reports whether
// they are exactly 1..n. inRange is false once p is past every list.
func partOrder(rest [][]int, p int, fromEnd bool, n int) (order []int, ok, inRange bool) {
	seen := make([]bool, n+1)
	order = make([]int, len(rest))
	ok = true
	for i, r := range rest {
		if p >= len(r) {
			ok = false
			continue
		}
		inRange = true
		v := r[p]
		if fromEnd {
			v = r[len(r)-1-p]
		}
		if v < 1 || v > n || seen[v] {
			ok = false
			continue
		}
		seen[v] = true
		order[i] = v
	}
	return order, ok && inRange, inRange
}

func contains(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func allContain(nums [][]int, v int) bool {
	for _, xs := range nums {
		if !contains(xs, v) {
			return false
		}
	}
	return true
}

func removeOnce(xs []int, v int) []int {
	out := make([]int, 0, len(xs))
	done := false
	for _, x := range xs {
		if x == v && !done {
			done = true
			continue
		}
		out = append(out, x)
	}
	return out
}

// commonTotal finds the smallest number above n present in every name: the
// total the files claim when some parts are missing.
func commonTotal(nums [][]int, n int) int {
	best := 0
	for _, v := range nums[0] {
		if v > n && v < 10000 && allContain(nums, v) && (best == 0 || v < best) {
			best = v
		}
	}
	return best
}

func missingList(nums [][]int, total int) string {
	have := map[int]bool{}
	for _, xs := range nums {
		for _, x := range removeOnce(xs, total) {
			have[x] = true
		}
	}
	var miss []string
	for i := 1; i <= total && len(miss) < 8; i++ {
		if !have[i] {
			miss = append(miss, strconv.Itoa(i))
		}
	}
	if len(miss) == 0 {
		return ""
	}
	return "; missing part " + strings.Join(miss, ", ")
}
