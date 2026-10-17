package send

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zero11-0203/s3mail/go/internal/model"
	"github.com/zero11-0203/s3mail/go/internal/relay"
)

type memStore struct {
	mu       sync.Mutex
	st       model.State
	subs     map[string]model.Submission
	quota    map[string]int64
	attempts []model.Attempt
	sends    []string
}

func (m *memStore) State(context.Context) (model.State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st, nil
}

func (m *memStore) GetSubmission(_ context.Context, id string) (model.Submission, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.subs[id]
	return s.Clone(), ok, nil
}

func (m *memStore) ListSubmissions(context.Context) ([]model.Submission, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []model.Submission
	for _, s := range m.subs {
		out = append(out, s.Clone())
	}
	return out, nil
}

func (m *memStore) Commit(_ context.Context, prev model.State, mu model.Mutation) (model.State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if prev != m.st {
		return model.State{}, model.ErrStateChanged
	}
	for _, q := range mu.Quota {
		if q.Units > 0 && m.quota[q.Key]+q.Units > q.Limit {
			return model.State{}, model.ErrOverQuota
		}
	}
	for _, q := range mu.Quota {
		m.quota[q.Key] += q.Units
	}
	for _, c := range mu.Submissions {
		m.subs[c.After.ID] = c.After.Clone()
	}
	m.attempts = append(m.attempts, mu.Attempts...)
	m.sends = append(m.sends, mu.Send...)
	m.st.Seq++
	return m.st, nil
}

type originals map[string][]byte

func (o originals) Fetch(_ context.Context, ref model.BlobRef) ([]byte, error) {
	return o[ref.Key], nil
}

type config struct {
	paused bool
	relay  relay.Relay
}

func (c *config) Paused(context.Context) (bool, error)       { return c.paused, nil }
func (c *config) Relay(context.Context) (relay.Relay, error) { return c.relay, nil }
func (q *queue) Enqueue(_ context.Context, id string, d time.Duration) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = append(q.items, queued{id, d})
	return nil
}

type queued struct {
	id    string
	delay time.Duration
}

type queue struct {
	mu    sync.Mutex
	items []queued
}

var t0 = time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)

const raw = "From: me@example.com\r\nTo: a@example.net\r\nBcc: hidden@example.net\r\nSubject: hi\r\n\r\nbody\r\n"

func setup(t *testing.T, results ...relay.Result) (*Worker, *memStore, *relay.Fake, *queue, *time.Time) {
	t.Helper()
	policy := Policy{Relay: "smtp2go", Daily: 10, PerRecipient: true}
	res, deltas := policy.Reserve(t0, 2)
	sub := model.Submission{ID: "Sa", EmailID: "Ma", MailFrom: "me@example.com", RcptTo: []string{"a@example.net", "hidden@example.net"},
		Blob: model.BlobRef{Key: "composed/Ma"}, Status: model.SubmissionQueued, NextAttemptAt: t0, Reservation: res}
	sub.SetRecipients("queued", "")
	st := &memStore{subs: map[string]model.Submission{"Sa": sub}, quota: map[string]int64{deltas[0].Key: 2}}
	fake := &relay.Fake{Results: results}
	q := &queue{}
	now := t0
	w := &Worker{Store: st, Originals: originals{"composed/Ma": []byte(raw)}, Config: &config{relay: fake}, Queue: q,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return now }}
	return w, st, fake, q, &now
}

func phases(st *memStore) string {
	var p []string
	for _, a := range st.attempts {
		p = append(p, a.Phase)
	}
	return strings.Join(p, ",")
}

func TestAcceptedOnce(t *testing.T) {
	w, st, fake, _, _ := setup(t)
	ctx := context.Background()
	if err := w.Process(ctx, "Sa"); err != nil {
		t.Fatal(err)
	}
	s := st.subs["Sa"]
	if s.Status != model.SubmissionAccepted || s.UndoStatus() != "final" || s.Recipients["a@example.net"].Delivered != "queued" {
		t.Errorf("after accept %+v", s)
	}
	if len(fake.Sent) != 1 || strings.Contains(string(fake.Sent[0].Body), "hidden@") || len(fake.Sent[0].Envelope.RcptTo) != 2 {
		t.Errorf("sent %+v", fake.Sent)
	}
	if p := phases(st); p != "claimed,handoff_started,accepted" {
		t.Errorf("attempt history %s", p)
	}
	// A duplicate wake-up sends nothing.
	if err := w.Process(ctx, "Sa"); err != nil || fake.Calls() != 1 {
		t.Errorf("duplicate: %v, %d calls", err, fake.Calls())
	}
	if st.quota["smtp2go#day#2026-10-17"] != 2 {
		t.Errorf("acceptance consumes the reservation: %v", st.quota)
	}
}

func TestRetryableThenRejected(t *testing.T) {
	w, st, fake, q, now := setup(t,
		relay.Result{Outcome: relay.NotAccepted, Stage: "final", Reply: "451 4.3.0 later"},
		relay.Result{Outcome: relay.Rejected, Stage: "rcpt", Reply: "a recipient was refused", Recipients: map[string]string{"a@example.net": "550 5.1.1 unknown"}})
	ctx := context.Background()
	if err := w.Process(ctx, "Sa"); err != nil {
		t.Fatal(err)
	}
	s := st.subs["Sa"]
	if s.Status != model.SubmissionQueued || s.UndoStatus() != "pending" || !s.NextAttemptAt.Equal(t0.Add(time.Minute)) || len(q.items) != 1 || q.items[0].delay != time.Minute {
		t.Fatalf("retry scheduling %+v %+v", s, q.items)
	}
	// Not due yet: a stray wake-up does nothing.
	_ = w.Process(ctx, "Sa")
	if fake.Calls() != 1 {
		t.Errorf("early wake-up sent")
	}
	*now = now.Add(time.Minute)
	if err := w.Process(ctx, "Sa"); err != nil {
		t.Fatal(err)
	}
	s = st.subs["Sa"]
	if s.Status != model.SubmissionRejected || s.Recipients["a@example.net"].SMTPReply != "550 5.1.1 unknown" || s.Recipients["a@example.net"].Delivered != "no" {
		t.Errorf("rejected %+v", s)
	}
	if st.quota["smtp2go#day#2026-10-17"] != 0 {
		t.Errorf("rejection releases the reservation: %v", st.quota)
	}
}

func TestUnknownIsNeverResent(t *testing.T) {
	w, st, fake, _, now := setup(t, relay.Result{Outcome: relay.Unknown, Stage: "final", Reply: "EOF"})
	ctx := context.Background()
	_ = w.Process(ctx, "Sa")
	if s := st.subs["Sa"]; s.Status != model.SubmissionUnknown || s.Recipients["a@example.net"].Delivered != "unknown" {
		t.Fatalf("unknown %+v", s)
	}
	*now = now.Add(time.Hour)
	_ = w.Process(ctx, "Sa")
	rec := &Recovery{Store: st, Config: &config{}, Queue: &queue{}, Log: w.Log, Now: w.Now}
	_ = rec.Run(ctx)
	if fake.Calls() != 1 {
		t.Errorf("unknown acceptance was resent")
	}
	// The owner checked the relay and authorizes a new attempt.
	m, err := Resolve(st.subs["Sa"], "retry", *now)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = st.Commit(ctx, st.st, *m)
	if len(st.sends) != 1 || st.subs["Sa"].Status != model.SubmissionQueued {
		t.Errorf("owner retry %+v", st.subs["Sa"])
	}
}

func TestLeaseRecovery(t *testing.T) {
	_, st, _, _, _ := setup(t)
	ctx := context.Background()
	q := &queue{}
	now := t0
	rec := &Recovery{Store: st, Config: &config{}, Queue: q, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return now }}
	claim, _ := Claim(st.subs["Sa"], "w1", now)
	_, _ = st.Commit(ctx, st.st, *claim)
	// Crash before handoff: retryable once the lease expires.
	_ = rec.Run(ctx)
	if st.subs["Sa"].Status != model.SubmissionActive {
		t.Fatal("live lease recovered")
	}
	now = now.Add(Lease)
	_ = rec.Run(ctx)
	if s := st.subs["Sa"]; s.Status != model.SubmissionQueued || len(q.items) != 1 {
		t.Fatalf("pre-handoff expiry %+v %v", s, q.items)
	}
	// Crash after handoff: unknown, never re-enqueued.
	claim, _ = Claim(st.subs["Sa"], "w2", now)
	_, _ = st.Commit(ctx, st.st, *claim)
	h, err := Handoff(st.subs["Sa"], "w2", 2, now)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = st.Commit(ctx, st.st, *h)
	now = now.Add(Lease)
	_ = rec.Run(ctx)
	if s := st.subs["Sa"]; s.Status != model.SubmissionUnknown || len(q.items) != 1 {
		t.Fatalf("post-handoff expiry %+v %v", s, q.items)
	}
	// The original worker's late acceptance evidence still resolves it.
	m, _ := Finish(st.subs["Sa"], "w2", 2, relay.Result{Outcome: relay.Accepted, Stage: "final", Reply: "250 ok"}, now)
	if m == nil {
		t.Fatal("owner evidence refused")
	}
	if _, stale := Finish(st.subs["Sa"], "w1", 1, relay.Result{Outcome: relay.Accepted}, now); stale != -1 {
		t.Error("a stale owner completed an attempt")
	}
	// A due queued submission whose wake-up was lost is re-enqueued.
	st.subs["Sb"] = model.Submission{ID: "Sb", Status: model.SubmissionQueued, NextAttemptAt: now.Add(-Grace)}
	q.items = nil
	_ = rec.Run(ctx)
	if len(q.items) != 1 || q.items[0].id != "Sb" {
		t.Errorf("lost wake-up %v", q.items)
	}
	rec.Config = &config{paused: true}
	q.items = nil
	_ = rec.Run(ctx)
	if len(q.items) != 0 {
		t.Error("paused sending re-enqueued work")
	}
}

// cancelingRelay cancels the submission during the attempt, before the
// handoff check, as a concurrent EmailSubmission/set would.
type cancelingRelay struct {
	st    *memStore
	inner relay.Relay
}

func (c *cancelingRelay) Submit(ctx context.Context, m relay.Message) relay.Result {
	cm, ok := Cancel(c.st.subs["Sa"], t0)
	if !ok {
		panic("cancel refused")
	}
	_, _ = c.st.Commit(ctx, c.st.st, *cm)
	return c.inner.Submit(ctx, m)
}

func TestCancelBeforeHandoffSendsNothing(t *testing.T) {
	w, st, fake, _, _ := setup(t)
	w.Config = &config{relay: &cancelingRelay{st: st, inner: fake}}
	if err := w.Process(context.Background(), "Sa"); err != nil {
		t.Fatal(err)
	}
	if s := st.subs["Sa"]; s.Status != model.SubmissionCanceled || s.UndoStatus() != "canceled" || len(fake.Sent) != 0 {
		t.Errorf("%+v sent=%d", s, len(fake.Sent))
	}
	if st.quota["smtp2go#day#2026-10-17"] != 0 {
		t.Errorf("cancellation releases quota: %v", st.quota)
	}
	if _, ok := Cancel(model.Submission{Status: model.SubmissionActive, Handoff: true}, t0); ok {
		t.Error("canceled after handoff")
	}
}

func TestPausedClaimsNothing(t *testing.T) {
	w, st, fake, _, _ := setup(t)
	w.Config = &config{paused: true, relay: fake}
	_ = w.Process(context.Background(), "Sa")
	if st.subs["Sa"].Status != model.SubmissionQueued || fake.Calls() != 0 {
		t.Error("paused worker sent")
	}
}

func TestReserve(t *testing.T) {
	p := Policy{Relay: "smtp2go", Daily: 200, Monthly: 1000, PerRecipient: true}
	r, d := p.Reserve(time.Date(2026, 10, 31, 23, 30, 0, 0, time.UTC), 3)
	if r.Units != 3 || len(d) != 2 || d[0].Key != "smtp2go#day#2026-10-31" || d[1].Key != "smtp2go#month#2026-10" || d[1].Limit != 1000 {
		t.Errorf("%+v %+v", r, d)
	}
	if rel := Release(r); rel[1].Units != -3 {
		t.Errorf("release %+v", rel)
	}
}
