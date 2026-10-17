package search

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 17, 6, 0, 0, 0, time.UTC)

func testDocs() []Doc {
	return []Doc{
		{EmailID: "Ma", ThreadID: "Ta", ReceivedAt: t0, From: "Jörg <j@example.net>", To: "alice@example.com",
			Subject: "Quarterly report", Body: "Numbers for the <third> quarter are attached.", AttachmentNames: "q3.pdf", HasAttachment: true},
		{EmailID: "Mb", ThreadID: "Tb", ReceivedAt: t0, From: "王小明 <wang@example.cn>", To: "alice@example.com", Bcc: "audit@example.com",
			Subject: "会议纪要", Body: "今天的会议讨论了预算。"},
		{EmailID: "Mc", ThreadID: "Tc", ReceivedAt: t0, From: "tanaka@example.jp", To: "alice@example.com",
			Subject: "週報", Body: "東京で会議がありました。ＲＥＰＯＲＴ"},
	}
}

func TestIndexMatchSnapshotAndDelete(t *testing.T) {
	dir := t.TempDir()
	x, err := Open(filepath.Join(dir, "work.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = x.Close() }()
	if err := x.Apply(testDocs(), nil, Meta{Generation: "g1", Seq: 7, Epoch: 1, Schema: SchemaVersion}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		c    Condition
		want []string
	}{
		{Condition{"text", "report"}, []string{"Ma", "Mc"}}, // full-width body text folds
		{Condition{"subject", "report"}, []string{"Ma"}},
		{Condition{"body", "会议"}, []string{"Mb"}},
		{Condition{"text", "会议"}, []string{"Mb"}},
		{Condition{"text", "会議"}, []string{"Mc"}},
		{Condition{"text", "东京"}, []string{}},
		{Condition{"from", "joerg"}, []string{}},
		{Condition{"from", "jorg"}, []string{"Ma"}}, // diacritics removed
		{Condition{"bcc", "audit"}, []string{"Mb"}},
		{Condition{"text", "audit"}, []string{"Mb"}},
		{Condition{"text", "q3"}, []string{"Ma"}},
		{Condition{"text", `"third quarter"`}, []string{"Ma"}},
		{Condition{"text", `"quarter third"`}, []string{}},
		{Condition{"hasAttachment", "true"}, []string{"Ma"}},
		{Condition{"text", "讨论 预算"}, []string{"Mb"}},
	}
	for _, tc := range cases {
		got, err := x.Match(tc.c)
		slices.Sort(got)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("%+v: %v %v, want %v", tc.c, got, err, tc.want)
		}
	}

	snap := filepath.Join(dir, "snap.db")
	if err := x.Snapshot(snap); err != nil {
		t.Fatal(err)
	}
	if err := x.Apply(nil, []string{"Mb"}, Meta{Generation: "g2", Seq: 8, Epoch: 1, Schema: SchemaVersion}); err != nil {
		t.Fatal(err)
	}
	if got, _ := x.Match(Condition{"text", "会议"}); len(got) != 0 {
		t.Errorf("deleted doc still matches: %v", got)
	}
	r, err := OpenReadOnly(snap)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if m, _ := r.Meta(); m != (Meta{Generation: "g1", Seq: 7, Epoch: 1, Schema: SchemaVersion}) {
		t.Errorf("snapshot meta %+v", m)
	}
	if got, _ := r.Match(Condition{"text", "会议"}); len(got) != 1 {
		t.Errorf("the snapshot keeps its generation: %v", got)
	}
	stored, _ := r.Stored([]string{"Ma", "Mz"})
	if len(stored) != 1 || stored["Ma"][0] != "Quarterly report" {
		t.Errorf("stored %v", stored)
	}
	// Corruption is detected.
	b, _ := os.ReadFile(snap)
	for i := 4096; i < len(b) && i < 8192; i++ {
		b[i] ^= 0xff
	}
	bad := filepath.Join(dir, "bad.db")
	_ = os.WriteFile(bad, b, 0o600)
	if Verify(bad) == nil {
		t.Error("a corrupted snapshot must fail verification")
	}
}

func TestHighlight(t *testing.T) {
	s := Highlight("Quarterly <Report>", "Numbers for the third quarter are attached & reviewed.", []string{"report quarter"})
	if s.Subject == nil || *s.Subject != "<mark>Quarter</mark>ly &lt;<mark>Report</mark>&gt;" {
		t.Errorf("subject %v", deref(s.Subject))
	}
	if s.Preview == nil || *s.Preview != "Numbers for the third <mark>quarter</mark> are attached &amp; reviewed." {
		t.Errorf("preview %v", deref(s.Preview))
	}
	cjk := Highlight("会议纪要", "今天的会议讨论了预算。", []string{"会议"})
	if *cjk.Subject != "<mark>会议</mark>纪要" || *cjk.Preview != "今天的<mark>会议</mark>讨论了预算。" {
		t.Errorf("cjk %v %v", deref(cjk.Subject), deref(cjk.Preview))
	}
	if none := Highlight("x", "y", []string{"zzz"}); none.Subject != nil || none.Preview != nil {
		t.Error("no match gives null fields")
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
