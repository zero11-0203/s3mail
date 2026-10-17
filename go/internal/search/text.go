// Package search maintains and queries the rebuildable full-text index
// (docs/specs/search.md): one SQLite FTS5 database published to S3 by a
// single indexer and read by the search function.
package search

import (
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

var fold = cases.Fold()

// Normalize applies NFKC (which also folds full-width forms) and Unicode
// case folding, identically for documents and queries.
func Normalize(s string) string {
	return fold.String(norm.NFKC.String(s))
}

// isCJK reports scripts written without spaces between words.
func isCJK(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) || r == 'ー' || r == '々'
}

// Tokens splits normalized text into index tokens. Runs of CJK characters
// become overlapping character bigrams (a lone character stays a unigram),
// so any substring of two or more characters matches as a phrase of
// bigrams; other text keeps unicode61's word splitting. Dictionary word
// segmentation is not used (see docs/specs/search.md#multilingual-text).
func Tokens(s string) []string {
	var out []string
	var word, cjk []rune
	flushWord := func() {
		if len(word) > 0 {
			out = append(out, string(word))
			word = word[:0]
		}
	}
	flushCJK := func() {
		switch {
		case len(cjk) == 1:
			out = append(out, string(cjk))
		case len(cjk) > 1:
			for i := 0; i+1 < len(cjk); i++ {
				out = append(out, string(cjk[i:i+2]))
			}
		}
		cjk = cjk[:0]
	}
	for _, r := range Normalize(s) {
		switch {
		case isCJK(r):
			flushWord()
			cjk = append(cjk, r)
		case unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.Is(unicode.Mn, r):
			flushCJK()
			word = append(word, r)
		default:
			flushWord()
			flushCJK()
		}
	}
	flushWord()
	flushCJK()
	return out
}

// IndexText is the FTS5 column value for a document field.
func IndexText(s string) string { return strings.Join(Tokens(s), " ") }

// MatchExpr turns a JMAP text query into an FTS5 expression over cols:
// every whitespace-separated term (or "quoted phrase") must match, and a
// term's tokens must appear consecutively. It returns "" when the query
// has no searchable tokens.
func MatchExpr(query string, cols []string) string {
	var clauses []string
	for _, term := range splitTerms(query) {
		toks := Tokens(term)
		if len(toks) == 0 {
			continue
		}
		quoted := make([]string, len(toks))
		for i, t := range toks {
			quoted[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
		}
		clauses = append(clauses, strings.Join(quoted, " + "))
	}
	if len(clauses) == 0 {
		return ""
	}
	expr := "(" + strings.Join(clauses, ") AND (") + ")"
	if len(cols) > 0 {
		expr = "{" + strings.Join(cols, " ") + "} : (" + expr + ")"
	}
	return expr
}

func splitTerms(q string) []string {
	var terms []string
	for len(q) > 0 {
		q = strings.TrimLeftFunc(q, unicode.IsSpace)
		if q == "" {
			break
		}
		if q[0] == '"' {
			end := strings.IndexByte(q[1:], '"')
			if end < 0 {
				terms = append(terms, q[1:])
				break
			}
			terms = append(terms, q[1:1+end])
			q = q[2+end:]
			continue
		}
		end := strings.IndexFunc(q, unicode.IsSpace)
		if end < 0 {
			end = len(q)
		}
		terms = append(terms, q[:end])
		q = q[end:]
	}
	return terms
}
