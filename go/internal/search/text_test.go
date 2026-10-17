package search

import (
	"slices"
	"testing"
)

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"ＡＢＣ１２３": "abc123", // full-width folds under NFKC
		"Straße": "strasse",
		"ﾃｽﾄ":    "テスト", // half-width katakana
		"Ⅻ":      "xii",
	} {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTokens(t *testing.T) {
	cases := map[string][]string{
		"Hello, World! 2026": {"hello", "world", "2026"},
		"会议纪要":               {"会议", "议纪", "纪要"},
		"東京で会議":              {"東京", "京で", "で会", "会議"},
		"한국어":                {"한국", "국어"},
		"本 report":           {"本", "report"},
		"café crème":         {"café", "crème"},
		"Re:週報(第3回)":         {"re", "週報", "第", "3", "回"},
		"e-mail foo@bar.com": {"e", "mail", "foo", "bar", "com"},
		"ＭｉｘｅｄＣＡＳＥ　全角スペース": {"mixedcase", "全角", "角ス", "スペ", "ペー", "ース"},
	}
	for in, want := range cases {
		if got := Tokens(in); !slices.Equal(got, want) {
			t.Errorf("Tokens(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMatchExpr(t *testing.T) {
	if got := MatchExpr(`会议 "Quarterly Report" x"y`, []string{"subject", "body"}); got != `{subject body} : (("会议") AND ("quarterly" + "report") AND ("x" + "y"))` {
		t.Errorf("expr %s", got)
	}
	if MatchExpr("  ,; ", nil) != "" {
		t.Error("punctuation-only query has no tokens")
	}
}
