package dispatch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/zero11-0203/s3mail/go/internal/model"
	"github.com/zero11-0203/s3mail/go/internal/store"
)

type outbox struct {
	entries []store.OutboxEntry
	acked   []string
}

func (o *outbox) Outbox(context.Context) ([]store.OutboxEntry, error) { return o.entries, nil }
func (o *outbox) AckOutbox(_ context.Context, key string) error {
	o.acked = append(o.acked, key)
	return nil
}

type waker struct {
	seqs []int64
	err  error
}

func (w *waker) Wake(_ context.Context, seq int64) error {
	w.seqs = append(w.seqs, seq)
	return w.err
}

type queue struct {
	ids []string
	err error
}

func (q *queue) Enqueue(_ context.Context, id string, _ time.Duration) error {
	if q.err != nil {
		return q.err
	}
	q.ids = append(q.ids, id)
	return nil
}

func TestDrain(t *testing.T) {
	o := &outbox{entries: []store.OutboxEntry{
		{Key: "OUTBOX#00000000000000000005#index", Destination: "index", IDs: []string{"Ma"}},
		{Key: "OUTBOX#00000000000000000005#events", Destination: "events"},
		{Key: "OUTBOX#00000000000000000007#submit", Destination: "submit", IDs: []string{"Sa"}},
		{Key: "OUTBOX#00000000000000000009#index", Destination: "index", IDs: []string{"Mb"}},
	}}
	w, q := &waker{}, &queue{}
	d := &Dispatcher{Outbox: o, Waker: w, Submit: q, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := d.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(w.seqs, []int64{9}) || !slices.Equal(q.ids, []string{"Sa"}) || len(o.acked) != 4 {
		t.Errorf("wakes %v sends %v acks %v", w.seqs, q.ids, o.acked)
	}

	// Failed hand-offs keep their entries.
	o.acked = nil
	w.err, q.err = errors.New("down"), errors.New("down")
	if err := d.Drain(context.Background()); err == nil {
		t.Error("failure not reported")
	}
	if !slices.Equal(o.acked, []string{"OUTBOX#00000000000000000005#events"}) {
		t.Errorf("acked despite failures: %v", o.acked)
	}
}

type publisher struct {
	events []model.Event
	err    error
}

func (p *publisher) Publish(_ context.Context, events []model.Event) error {
	if p.err != nil {
		return p.err
	}
	p.events = append(p.events, events...)
	return nil
}

func TestDrainPublishesEvents(t *testing.T) {
	ev := model.Event{ID: "Ma:received", Type: "EmailReceived", EmailID: "Ma", MailboxIDs: []string{"inbox"}}
	o := &outbox{entries: []store.OutboxEntry{
		{Key: "OUTBOX#00000000000000000005#events", Destination: "events", Events: []model.Event{ev}},
	}}
	p := &publisher{err: errors.New("down")}
	d := &Dispatcher{Outbox: o, Events: p, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	// A failed publish keeps the entry for the next drain.
	if err := d.Drain(context.Background()); err == nil || len(o.acked) != 0 {
		t.Fatalf("err %v acks %v", err, o.acked)
	}
	p.err = nil
	if err := d.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(p.events) != 1 || p.events[0].ID != "Ma:received" || len(o.acked) != 1 {
		t.Errorf("published %+v acks %v", p.events, o.acked)
	}
}
