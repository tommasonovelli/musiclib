package importer

import (
	"path"
	"strings"
)

// naturalCompare is the natural order of §7.3 on two names: runs of ASCII
// digits compare as integers (of any length, leading zeros ignored), every
// other run compares by its UTF-8 bytes, and a digit run against a
// non-digit run compares by bytes. It returns 0 for names that differ only
// in leading zeros ("01" and "1"); callers break that tie with the full
// path (pathNaturalCompare), so the order is total.
func naturalCompare(a, b string) int {
	for a != "" && b != "" {
		ra, restA := nextRun(a)
		rb, restB := nextRun(b)
		da, db := isDigit(ra[0]), isDigit(rb[0])
		var c int
		if da && db {
			c = compareDigits(ra, rb)
		} else {
			c = strings.Compare(ra, rb)
		}
		if c != 0 {
			return c
		}
		a, b = restA, restB
	}
	switch {
	case a == "" && b == "":
		return 0
	case a == "":
		return -1
	default:
		return 1
	}
}

// pathNaturalCompare orders source paths by the natural order of their
// basenames, then by the bytes of the full path (§7.3: "i pareggi usano il
// percorso UTF-8 completo"). It is a total order on distinct paths.
func pathNaturalCompare(a, b string) int {
	if c := naturalCompare(path.Base(a), path.Base(b)); c != 0 {
		return c
	}
	return strings.Compare(a, b)
}

func isDigit(c byte) bool { return '0' <= c && c <= '9' }

// nextRun splits s into its first run (all digits or no digits) and the
// rest. s is not empty.
func nextRun(s string) (run, rest string) {
	d := isDigit(s[0])
	i := 1
	for i < len(s) && isDigit(s[i]) == d {
		i++
	}
	return s[:i], s[i:]
}

// compareDigits compares two runs of ASCII digits as integers.
func compareDigits(a, b string) int {
	a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}
