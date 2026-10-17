package dispatch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

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
