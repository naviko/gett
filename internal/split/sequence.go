package split

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gett/internal/errs"
)

const (
	digits  = "0123456789"
	letters = "abcdefghijklmnopqrstuvwxyz"
)

// set is the parts of one split file, in order.
type set struct {
	parts []string // paths
	name  string   // what the joined file is called
}

// The names of archive volumes. They look like parts of a split file but
// each carries its own headers, so putting them end to end gives nothing.
var volumeRe = regexp.MustCompile(`(?i)(\.part\d+\.rar|\.[rsz]\d\d)$`)

var (
	partDigitsRe = regexp.MustCompile(`(?i)^(.+)(\.part)(\d+)$`)
	dotDigitsRe  = regexp.MustCompile(`^(.+)(\.)(\d+)$`)
	digitsRe     = regexp.MustCompile(`\d+$`)
	lettersRe    = regexp.MustCompile(`[a-z]+$`)
)

// find works out which set of parts path belongs to. path is any part, or
// the name the parts were made from.
func find(path string) (*set, error) {
	dir, base := filepath.Dir(path), filepath.Base(path)
	if volumeRe.MatchString(base) {
		return nil, &errs.Archive{Msg: path + " is an archive volume, not a split file"}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, e := range entries {
		if e.Type().IsRegular() {
			have[e.Name()] = true
		}
	}
	if have[base] {
		if s, err := partOf(dir, base, have); s != nil || err != nil {
			return s, err
		}
	} else if _, err := os.Stat(path); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	// Not a part itself: the name the parts were made from, perhaps.
	for _, first := range []string{".001", ".000", ".01", ".00", ".part1", ".part01", ".part001", ".aa", "aa", "00"} {
		if have[base+first] {
			if s, err := partOf(dir, base+first, have); s != nil || err != nil {
				return s, err
			}
		}
	}
	if !have[base] {
		if _, err := os.Stat(path); err != nil {
			return nil, err
		}
	}
	return nil, &errs.Archive{Msg: path + " is not part of a split file"}
}

// partOf looks at base as a part: a prefix followed by a counter. It returns
// nil when base is no part of anything.
func partOf(dir, base string, have map[string]bool) (*set, error) {
	// Digits after a dot or ".part": a counter is the only reading.
	for _, re := range []*regexp.Regexp{partDigitsRe, dotDigitsRe} {
		m := re.FindStringSubmatch(base)
		if m == nil {
			continue
		}
		name, sep, suffix := m[1], m[2], m[3]
		prefix := name + sep
		least := 2 // parts a set must have before the name is believed
		if sep == "." && len(suffix) >= 3 {
			least = 1 // name.001 alone is a whole split file that needed one part
		}
		s, err := walk(dir, prefix, suffix, digits, starts(digits, 1, len(suffix)), least, have)
		if s != nil || err != nil {
			if s != nil {
				s.name = outName(name, sep)
			}
			return s, err
		}
		if len(suffix) >= 2 && alike(prefix, len(suffix), have) >= 2 {
			return nil, &errs.Archive{Msg: fmt.Sprintf("%s is missing, nothing joined", filepath.Join(dir, prefix+strings.Repeat("0", len(suffix)-1)+"1"))}
		}
		return nil, nil
	}

	// Digits or letters straight after the name, as split writes them. Where
	// the name ends is not marked, and split lengthens its counter as it
	// goes, so every way of cutting the counter off is tried and the one that
	// gives the longest sequence is right.
	var best *set
	var gap error
	for alphabet, tail := range map[string]string{digits: digitsRe.FindString(base), letters: lettersRe.FindString(base)} {
		for n := 2; n <= len(tail) && n <= 8 && n < len(base); n++ {
			prefix, suffix := base[:len(base)-n], base[len(base)-n:]
			s, err := walk(dir, prefix, suffix, alphabet, starts(alphabet, 2, n), 2, have)
			if err != nil && gap == nil {
				gap = err
			}
			if s != nil && (best == nil || len(s.parts) > len(best.parts)) {
				s.name = outName(prefix, "")
				best = s
			}
		}
	}
	if best != nil {
		return best, nil
	}
	return nil, gap
}

// starts lists the counters a sequence can begin with, for every width from
// low to high: zeros, or for digits also zeros and a one.
func starts(alphabet string, low, high int) []string {
	var out []string
	for w := low; w <= high; w++ {
		out = append(out, strings.Repeat(alphabet[:1], w))
		if alphabet == digits {
			out = append(out, strings.Repeat("0", w-1)+"1")
		}
	}
	return out
}

// outName is what the joined file is called: the part's name without the
// counter and what joined the two.
func outName(name, sep string) string {
	if sep == "" {
		name = strings.TrimRight(name, "._-")
	}
	if name == "" {
		return "joined"
	}
	return name
}

// walk follows a sequence from the first of starts that exists. It returns
// nil when there is no sequence of at least least parts holding suffix, and
// an error when there is one with a part missing.
func walk(dir, prefix, suffix, alphabet string, starts []string, least int, have map[string]bool) (*set, error) {
	for _, start := range starts {
		if !have[prefix+start] {
			continue
		}
		used := map[string]bool{start: true}
		parts := []string{filepath.Join(dir, prefix+start)}
		cur := start
	follow:
		for {
			for _, n := range next(cur, alphabet) {
				if have[prefix+n] {
					cur = n
					used[n] = true
					parts = append(parts, filepath.Join(dir, prefix+n))
					continue follow
				}
			}
			break
		}
		if !used[suffix] && !beyond(prefix, cur, alphabet, have) {
			continue // another sequence; the file asked about is not in it
		}
		// A part further along the counter means one in between is missing.
		if beyond(prefix, cur, alphabet, have) {
			return nil, &errs.Archive{Msg: fmt.Sprintf("%s is missing, nothing joined", filepath.Join(dir, prefix+next(cur, alphabet)[0]))}
		}
		if len(parts) < least {
			continue
		}
		return &set{parts: parts}, nil
	}
	return nil, nil
}

// reach is how far past the end of a sequence a later part is looked for.
const reach = 1000

// beyond reports whether a file exists whose counter comes after last with
// others missing in between. Only exact names along the counter are tried,
// so a file that merely starts the same way is never taken for a part.
func beyond(prefix, last, alphabet string, have map[string]bool) bool {
	cur := last
	for i := 0; i <= reach; i++ {
		ns := next(cur, alphabet)
		if len(ns) == 0 {
			return false
		}
		if i > 0 {
			for _, n := range ns {
				if have[prefix+n] {
					return true
				}
			}
		}
		cur = ns[0]
	}
	return false
}

// alike counts the files named prefix plus n digits.
func alike(prefix string, n int, have map[string]bool) int {
	count := 0
	for name := range have {
		if s, ok := strings.CutPrefix(name, prefix); ok && len(s) == n && strings.Trim(s, digits) == "" {
			count++
		}
	}
	return count
}

// next lists the counters that can follow s, likeliest first:
//
//   - one higher at the same width: 001 to 002, aa to ab;
//   - a digit wider when the width is used up, as 7-Zip does: 999 to 1000;
//   - the longer counter split moves to rather than let the first character
//     reach the end of the alphabet: 89 to 9000, yz to zaaa, 9899 to 990000.
func next(s, alphabet string) []string {
	first, last := alphabet[0], alphabet[len(alphabet)-1]
	var out []string
	add := func(n string) {
		for _, have := range out {
			if have == n {
				return
			}
		}
		out = append(out, n)
	}
	plain, over := increment(s, alphabet)
	if !over {
		add(plain)
	} else if alphabet == digits {
		add("1" + strings.Repeat("0", len(s)))
	}

	// split keeps the characters it has already used up at the front and
	// counts with the rest.
	kept := len(s) - len(strings.TrimLeft(s, string(last)))
	if rest := s[kept:]; rest != "" {
		if n, over := increment(rest, alphabet); !over {
			if n[0] == last {
				add(s[:kept] + string(last) + strings.Repeat(string(first), len(rest)+1))
			} else {
				add(s[:kept] + n)
			}
		}
	}
	return out
}

// increment adds one to a counter of fixed width. over reports that it was
// at its highest.
func increment(s, alphabet string) (n string, over bool) {
	b := []byte(s)
	for i := len(b) - 1; i >= 0; i-- {
		at := strings.IndexByte(alphabet, b[i])
		if at < len(alphabet)-1 {
			b[i] = alphabet[at+1]
			return string(b), false
		}
		b[i] = alphabet[0]
	}
	return string(b), true
}
