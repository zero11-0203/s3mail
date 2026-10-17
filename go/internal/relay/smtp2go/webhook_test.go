package smtp2go

import "testing"

func TestParse(t *testing.T) {
	evs, err := Parse([]byte(`[
		{"event":"processed","email_id":"1a-2b-3c","rcpt":"a@example.net"},
		{"event":"delivered","email_id":"1a-2b-3c","rcpt":"a@example.net","time":"2026-10-17T05:22:30Z","message":"250 2.0.0 OK"},
		{"event":"bounce","bounce":"hard","email_id":"1a-2b-3c","rcpt":"b@example.net","host":"mx.example.net","context":"550 5.1.1 unknown user"},
		{"event":"bounce","bounce":"soft","email_id":"1a-2b-3c","rcpt":"c@example.net","message":"452 mailbox full"},
		{"event":"reject","email_id":"1a-2b-3c","rcpt":"d@example.net","message":"recipient suppressed"},
		{"event":"open","email_id":"1a-2b-3c","rcpt":"a@example.net"}]`))
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ rcpt, delivered, reply string }{
		{"a@example.net", "yes", "250 2.0.0 OK"},
		{"b@example.net", "no", "550 5.1.1 unknown user (mx.example.net)"},
		{"c@example.net", "queued", "452 mailbox full"},
		{"d@example.net", "no", "recipient suppressed"},
	}
	if len(evs) != len(want) {
		t.Fatalf("events %+v", evs)
	}
	for i, w := range want {
		e := evs[i]
		if e.Recipient != w.rcpt || e.Delivered != w.delivered || e.Reply != w.reply || e.RelayID != "1a-2b-3c" || len(e.ID) != 32 {
			t.Errorf("%d: %+v", i, e)
		}
	}
	if evs[0].At.IsZero() {
		t.Error("time not parsed")
	}
	// The same report has the same id; another report does not.
	again, _ := Parse([]byte(`{"event":"delivered","email_id":"1a-2b-3c","rcpt":"A@example.net","time":"2026-10-17T05:22:30Z","message":"250 2.0.0 OK"}`))
	if again[0].ID != evs[0].ID || again[0].ID == evs[1].ID {
		t.Error("event ids")
	}
	for _, bad := range []string{"", "not json", `{"rcpt":"x"}`, `[1]`} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
