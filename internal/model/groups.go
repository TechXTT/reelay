package model

import (
	"slices"
	"strings"
)

// CaseDuplicateGroups returns the preferred-group names that collide once
// lowercased, e.g. "NTb" and "ntb". Scoring matches groups case-insensitively,
// so such keys are ambiguous. Each result lists the original keys sorted; the
// results are ordered by lowercase name so callers get deterministic output.
func CaseDuplicateGroups(groups map[string]int) [][]string {
	byLower := map[string][]string{}
	for name := range groups {
		lower := strings.ToLower(name)
		byLower[lower] = append(byLower[lower], name)
	}

	var lowers []string
	for lower, names := range byLower {
		if len(names) > 1 {
			lowers = append(lowers, lower)
		}
	}
	slices.Sort(lowers)

	out := make([][]string, 0, len(lowers))
	for _, lower := range lowers {
		names := byLower[lower]
		slices.Sort(names)
		out = append(out, names)
	}
	return out
}
