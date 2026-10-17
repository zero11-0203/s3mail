package search

import (
	"html"
	"slices"
	"strings"
	"unicode"
)

// Snippet is a SearchSnippet's highlighted strings (RFC 8621 §5): message
// text is HTML-escaped and only <mark></mark> is inserted. A field without
// a match is nil.
type Snippet struct {
	Subject *string `json:"subject"`
	Preview *string `json:"preview"`
}

const previewRunes = 255

// Highlight builds the snippet of subject and body for the query terms.
func Highlight(subject, body string, terms []string) Snippet {
	needles := needlesOf(terms)
	var s Snippet
	if spans := find([]rune(subject), needles); len(spans) > 0 {
		v := mark([]rune(subject), spans)
		s.Subject = &v
	}
	text := []rune(strings.Join(strings.Fields(body), " "))
	if spans := find(text, needles); len(spans) > 0 {
		start := max(spans[0][0]-40, 0)
		end := min(start+previewRunes, len(text))
		var inside [][2]int
		for _, sp := range spans {
			if sp[0] >= start && sp[1] <= end {
				inside = append(inside, [2]int{sp[0] - start, sp[1] - start})
			}
		}
		v := mark(text[start:end], inside)
		s.Preview = &v
	}
	return s
}

// needlesOf returns the lower-cased literal strings to highlight: each
// query term, and each CJK run within it.
func needlesOf(terms []string) [][]rune {
	var out [][]rune
	for _, t := range terms {
		for _, part := range splitTerms(t) {
			for _, f := range strings.FieldsFunc(part, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) }) {
				if n := []rune(strings.ToLower(f)); len(n) > 0 && !slices.ContainsFunc(out, func(o []rune) bool { return slices.Equal(o, n) }) {
					out = append(out, n)
				}
			}
		}
	}
	return out
}

// find returns the non-overlapping spans of any needle in text, compared
// rune by rune in lower case.
func find(text []rune, needles [][]rune) [][2]int {
	var spans [][2]int
	for i := 0; i < len(text); {
		best := 0
		for _, n := range needles {
			if len(n) > best && i+len(n) <= len(text) && lowerEqual(text[i:i+len(n)], n) {
				best = len(n)
			}
		}
		if best > 0 {
			spans = append(spans, [2]int{i, i + best})
			i += best
		} else {
			i++
		}
	}
	return spans
}

func lowerEqual(a, b []rune) bool {
	for i := range a {
		if unicode.ToLower(a[i]) != b[i] {
			return false
		}
	}
	return true
}

func mark(text []rune, spans [][2]int) string {
	var b strings.Builder
	at := 0
	for _, sp := range spans {
		b.WriteString(html.EscapeString(string(text[at:sp[0]])))
		b.WriteString("<mark>")
		b.WriteString(html.EscapeString(string(text[sp[0]:sp[1]])))
		b.WriteString("</mark>")
		at = sp[1]
	}
	b.WriteString(html.EscapeString(string(text[at:])))
	return b.String()
}
