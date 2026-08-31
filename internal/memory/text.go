package memory

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// Text similarity for recall, done in process so the same ranking works on SQLite
// and Postgres and needs no database extension.

// trigrams returns the set of 3-grams of a text the way pg_trgm builds them: the
// text is lowercased and split into alphanumeric words, and each word is padded
// with two spaces in front and one behind.
func trigrams(text string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, w := range words(text) {
		padded := []rune("  " + w + " ")
		for i := 0; i+3 <= len(padded); i++ {
			out[string(padded[i:i+3])] = struct{}{}
		}
	}
	return out
}

func words(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// similarity is shared trigrams over all trigrams, in [0, 1].
func similarity(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	shared := 0
	small, large := a, b
	if len(b) < len(a) {
		small, large = b, a
	}
	for g := range small {
		if _, ok := large[g]; ok {
			shared++
		}
	}
	return float64(shared) / float64(len(a)+len(b)-shared)
}

var stopwords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`a about after all also am an and any are as at be because been but by can could did do does for from had
		has have how i if in into is it its just me more most my no not of on or our out over so some such than that the their them then there
		these they this to too up us was we were what when where which who why will with would you your`) {
		m[w] = true
	}
	return m
}()

// stem is a light suffix stripper, enough to match crash and crashing, pods and pod.
func stem(w string) string {
	switch {
	case len(w) > 5 && strings.HasSuffix(w, "ing"):
		return w[:len(w)-3]
	case len(w) > 4 && strings.HasSuffix(w, "ies"):
		return w[:len(w)-3] + "y"
	case len(w) > 4 && strings.HasSuffix(w, "ed"):
		return w[:len(w)-2]
	case len(w) > 3 && strings.HasSuffix(w, "es"):
		return w[:len(w)-2]
	case len(w) > 3 && strings.HasSuffix(w, "s"):
		return w[:len(w)-1]
	}
	return w
}

// terms returns the content terms of a text: lowercase words, stopwords dropped,
// light stemming. Duplicates are kept so a term's frequency is known.
func terms(text string) []string {
	var out []string
	for _, w := range words(text) {
		if !stopwords[w] {
			out = append(out, stem(w))
		}
	}
	return out
}

// lexicalScore is the full text channel. It admits a document that carries ANY
// query term, and ranks by how many query terms it has and how often. The channel
// is a ranking input to rank fusion, not a filter: an AND query would demand that
// one summary contain every content word of a natural language question, which it
// never does, and the channel would return nothing at all.
func lexicalScore(query map[string]struct{}, doc []string) float64 {
	if len(query) == 0 {
		return 0
	}
	tf := map[string]int{}
	for _, t := range doc {
		tf[t]++
	}
	score := 0.0
	for q := range query {
		if n := tf[q]; n > 0 {
			score += 1 + math.Log(float64(n))
		}
	}
	return score
}

func termSet(ts []string) map[string]struct{} {
	m := make(map[string]struct{}, len(ts))
	for _, t := range ts {
		m[t] = struct{}{}
	}
	return m
}

// rrfK is the reciprocal rank fusion constant (Cormack et al. 2009).
const rrfK = 60

type ranked struct {
	id    string
	score float64
	at    float64
}

// rank orders by score descending, then recency descending, and returns ids in order.
func rank(items []ranked) []ranked {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].score != items[j].score {
			return items[i].score > items[j].score
		}
		return items[i].at > items[j].at
	})
	return items
}
