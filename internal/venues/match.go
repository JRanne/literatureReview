package venues

import (
	"regexp"
	"strings"
)

var (
	parenRe   = regexp.MustCompile(`\(([^)]*)\)`)
	ordinalRe = regexp.MustCompile(`\b\d{1,3}(st|nd|rd|th)\b`)
	yearRe    = regexp.MustCompile(`\b(19|20)\d{2}\b`)
	acroRe    = regexp.MustCompile(`\b[A-Z][A-Z0-9&-]{2,9}\b`)
	noiseRe   = regexp.MustCompile(`\b(proceedings|proceeding|proc|of|the|annual|international|conference|on|in|and|workshop|symposium|companion|volume|vol|part|issue)\b`)
)

// Canonical strips conference noise ("Proceedings of the 32nd … (ECIS 2024)")
// so that names can be compared with registries; it also returns any
// acronym found in parentheses or as an all-caps token.
func Canonical(venue string) (name string, acronym string) {
	v := strings.ReplaceAll(venue, "&amp;", "&")
	for _, m := range parenRe.FindAllStringSubmatch(v, -1) {
		inner := strings.TrimSpace(m[1])
		if a := acroRe.FindString(inner); a != "" && len(a) <= 10 {
			acronym = a
		}
	}
	v = parenRe.ReplaceAllString(v, " ")
	if acronym == "" {
		for _, a := range acroRe.FindAllString(v, -1) {
			if a != "IEEE" && a != "ACM" && a != "AIS" && len(a) >= 3 {
				acronym = a
				break
			}
		}
	}
	v = ordinalRe.ReplaceAllString(v, " ")
	v = yearRe.ReplaceAllString(v, " ")
	name = norm(v)
	return name, acronym
}

// tokens returns the informative words of a normalised name.
func tokens(n string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.Fields(noiseRe.ReplaceAllString(n, " ")) {
		if len(w) > 1 {
			out[w] = true
		}
	}
	return out
}

// Similarity is the Jaccard overlap of informative tokens (0..1).
func Similarity(a, b string) float64 {
	ta, tb := tokens(norm(a)), tokens(norm(b))
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	inter := 0
	for w := range ta {
		if tb[w] {
			inter++
		}
	}
	return float64(inter) / float64(len(ta)+len(tb)-inter)
}

// bestMatch picks the candidate most similar to venue, if similar enough.
func bestMatch(venue string, cands []string, min float64) (int, float64) {
	best, bestSim := -1, 0.0
	for i, c := range cands {
		if s := Similarity(venue, c); s > bestSim {
			best, bestSim = i, s
		}
	}
	if bestSim < min {
		return -1, bestSim
	}
	return best, bestSim
}
