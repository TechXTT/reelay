package parser

import (
	"regexp"
	"strings"
	"unicode"
)

var multiSpaceRe = regexp.MustCompile(`\s+`)

// NormalizeTitle produces the form titles are compared on: lowercase, ampersand
// expanded, punctuation dropped, whitespace collapsed.
//
// The leading article is deliberately KEPT here — "The Office" and "Office" are
// different shows — and stripped only by NormalizeForMatch, which is used as a
// fallback comparison rather than as the primary key.
func NormalizeTitle(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "&", " and ")
	s = strings.ReplaceAll(s, "+", " and ")

	// Drop apostrophes without leaving a gap, so "marvel's" becomes "marvels"
	// rather than "marvel s".
	s = strings.Map(func(r rune) rune {
		if r == '\'' || r == '’' || r == '`' {
			return -1
		}
		return r
	}, s)

	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case unicode.IsSpace(r):
			b.WriteRune(' ')
		default:
			// Any other punctuation becomes a separator: "spider-man" and
			// "spider man" must normalise identically.
			b.WriteRune(' ')
		}
	}
	return strings.TrimSpace(multiSpaceRe.ReplaceAllString(b.String(), " "))
}

var leadingArticleRe = regexp.MustCompile(`^(?:the|a|an)\s+`)

// NormalizeForMatch is NormalizeTitle with the leading article removed. Used
// as a secondary comparison because indexers are inconsistent about it
// ("The Expanse" vs "Expanse").
func NormalizeForMatch(s string) string {
	return leadingArticleRe.ReplaceAllString(NormalizeTitle(s), "")
}

// Levenshtein is the edit distance between a and b, computed with a single
// rolling row.
//
// Bounded by max: once every cell in a row exceeds max the answer cannot come
// back down, so the walk stops early. That turns the common "these are not the
// same show at all" case from O(len(a)*len(b)) into a couple of rows.
func Levenshtein(a, b string, max int) int {
	if a == b {
		return 0
	}
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 {
		return len(rb)
	}
	if len(rb) == 0 {
		return len(ra)
	}
	// A length difference alone already exceeds the budget.
	if diff := len(ra) - len(rb); diff > max || -diff > max {
		return max + 1
	}

	prev := make([]int, len(rb)+1)
	curr := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}

	for i := 1; i <= len(ra); i++ {
		curr[0] = i
		rowMin := curr[0]
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			curr[j] = min3(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
			if curr[j] < rowMin {
				rowMin = curr[j]
			}
		}
		if rowMin > max {
			return max + 1
		}
		prev, curr = curr, prev
	}
	return prev[len(rb)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

// fuzzyBudget is the edit distance allowed for a title of the given length.
//
// A flat "distance <= 2" is dangerous for short titles: "Us" and "Up" are one
// edit apart, and so are "Alone" and "Alone" variants of entirely different
// shows. Scaling with length keeps the tolerance where it is useful — long
// titles where a typo or a transliteration differs by a character or two — and
// removes it where it would produce false matches.
func fuzzyBudget(n int) int {
	switch {
	case n < 8:
		return 0
	case n < 14:
		return 1
	default:
		return 2
	}
}

// SortTitle produces a library-ordering key: normalised, article moved to the
// end in the conventional way.
func SortTitle(s string) string {
	n := NormalizeTitle(s)
	for _, art := range []string{"the ", "a ", "an "} {
		if strings.HasPrefix(n, art) {
			return strings.TrimSpace(n[len(art):]) + ", " + strings.TrimSpace(art)
		}
	}
	return n
}
