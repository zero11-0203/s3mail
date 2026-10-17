// Package dispatch drains the transactional outbox
// (docs/specs/storage-sync.md#idempotency-and-durable-work). A DynamoDB
// stream on outbox inserts triggers a drain within seconds; a slower
// scheduled sweep drains whatever a failed trigger left behind. Every
// destination tolerates duplicates, so concurrent drains are safe: work is
// handed on before its entry is acknowledged.
package dispatch

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/zero11-0203/s3mail/go/internal/search"
	"github.com/zero11-0203/s3mail/go/internal/store"
)

// Outbox reads and acknowledges entries.
type Outbox interface {
	Outbox(ctx context.Context) ([]store.OutboxEntry, error)
	AckOutbox(ctx context.Context, key string) error
}

// Queue hands a submission to the submission queue.
type Queue interface {
	Enqueue(ctx context.Context, submissionID string, delay time.Duration) error
}

// Dispatcher routes entries by destination.
type Dispatcher struct {
	Outbox Outbox
	// Search and Waker run the index safety check on sweeps.
	Search search.Source
	Waker  search.Waker
	Submit Queue
	Log    *slog.Logger
}

// entrySeq is the commit sequence in OUTBOX#<seq>#<destination>.
func entrySeq(key string) int64 {
	parts := strings.Split(key, "#")
	if len(parts) < 3 {
		return 0
	}
	n, _ := strconv.ParseInt(parts[1], 10, 64)
	return n
}

// Drain hands every pending entry to its destination and acknowledges it.
// An entry whose hand-off fails stays for the next drain.
func (d *Dispatcher) Drain(ctx context.Context) error {
	entries, err := d.Outbox.Outbox(ctx)
	if err != nil {
		return err
	}
	var errs []error
	ack := func(e store.OutboxEntry) {
		if err := d.Outbox.AckOutbox(ctx, e.Key); err != nil {
			errs = append(errs, err)
		}
	}
	// Index entries coalesce into one wake-up: the indexer replays from
	// its own cursor.
	var index []store.OutboxEntry
	var top int64
	for _, e := range entries {
		switch e.Destination {
		case "index":
			index = append(index, e)
			top = max(top, entrySeq(e.Key))
		case "submit":
			ok := true
			for _, id := range e.IDs {
				if err := d.Submit.Enqueue(ctx, id, 0); err != nil {
					errs = append(errs, err)
					ok = false
				}
			}
			if ok {
				ack(e)
			}
		case "events":
			// No event bus consumes EmailReceived yet; the entry is
			// acknowledged without publishing.
			ack(e)
		default:
			d.Log.Warn("unknown outbox destination", "key", e.Key)
		}
	}
	if len(index) > 0 {
		if err := d.Waker.Wake(ctx, top); err != nil {
			errs = append(errs, err)
		} else {
			for _, e := range index {
				ack(e)
			}
		}
	}
	d.Log.Debug("drained", "entries", len(entries), "index", len(index))
	return errors.Join(errs...)
}

// Sweep drains and then checks the index against the change log, which
// also covers index work committed before the outbox carried it.
func (d *Dispatcher) Sweep(ctx context.Context) error {
	err := d.Drain(ctx)
	woke, serr := search.Dispatch(ctx, d.Search, d.Waker)
	if woke {
		d.Log.Info("index behind the change log; woke the indexer")
	}
	return errors.Join(err, serr)
}
