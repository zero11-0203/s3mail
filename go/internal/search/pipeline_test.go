package search

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/zero11-0203/s3mail/go/internal/model"
)

// memSource is an account with the guarded commit the pipeline relies on.
type memSource struct {
	mu      sync.Mutex
	st      model.State
	emails  map[string]model.Email
	raw     map[string][]byte
	changes []model.Change
	reg     *model.SearchIndex
	failReg int // fail the next n registrations after the check
	now     time.Time
}

func (m *memSource) State(context.Context) (model.State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.st, nil
}

func (m *memSource) Changes(_ context.Context, after, upto int64) ([]model.Change, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []model.Change
	for _, c := range m.changes {
		if c.Seq > after && c.Seq <= upto {
			out = append(out, c)
		}
	}
	return out, nil
}

func (m *memSource) ListEmails(context.Context) ([]model.Email, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []model.Email
	for _, e := range m.emails {
		out = append(out, e)
	}
	return out, nil
}

func (m *memSource) GetEmails(_ context.Context, ids []string) (map[string]model.Email, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]model.Email{}
	for _, id := range ids {
		if e, ok := m.emails[id]; ok {
			out[id] = e
		}
	}
	return out, nil
}

func (m *memSource) SearchIndex(context.Context) (model.SearchIndex, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.reg == nil {
		return model.SearchIndex{}, false, nil
	}
	return *m.reg, true, nil
}

func (m *memSource) Commit(_ context.Context, prev model.State, mu model.Mutation) (model.State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if prev != m.st {
		return model.State{}, model.ErrStateChanged
	}
	r := mu.Search
	cur := ""
	if m.reg != nil {
		cur = m.reg.Generation
	}
	if r.Prev != cur {
		return model.State{}, model.ErrSearchConflict
	}
	if m.failReg > 0 {
		m.failReg--
		return model.State{}, errors.New("injected crash")
	}
	next := r.Next
	m.reg = &next
	m.st.Seq++
	m.st.Email = m.st.Seq
	m.changes = append(m.changes, model.Change{Seq: m.st.Seq, At: m.now})
	return m.st, nil
}

func (m *memSource) Fetch(_ context.Context, ref model.BlobRef) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.raw[ref.Key], nil
}

func (m *memSource) add(id, subject, body string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.st.Seq++
	m.st.Email = m.st.Seq
	m.emails[id] = model.Email{ID: id, ThreadID: "T" + id[1:], ReceivedAt: t0, Blob: model.BlobRef{Key: id}}
	m.raw[id] = []byte("Subject: " + subject + "\r\nFrom: a@example.net\r\n\r\n" + body + "\r\n")
	m.changes = append(m.changes, model.Change{Seq: m.st.Seq, Created: map[string][]string{"Email": {id}}, At: m.now})
}

func (m *memSource) destroy(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.st.Seq++
	m.st.Email = m.st.Seq
	delete(m.emails, id)
	m.changes = append(m.changes, model.Change{Seq: m.st.Seq, Destroyed: map[string][]string{"Email": {id}}, At: m.now})
}

func (m *memSource) touch() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.st.Seq++
	m.changes = append(m.changes, model.Change{Seq: m.st.Seq, Updated: map[string][]string{"Email": {"Mx"}}, At: m.now})
}

// memObjects is a versioned bucket with conditional writes.
type memObjects struct {
	mu       sync.Mutex
	versions [][]byte
	uploads  int
}

func (o *memObjects) etag(i int) string { return fmt.Sprintf("etag-%d", i) }

func (o *memObjects) Head(context.Context, string) (Object, bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.versions) == 0 {
		return Object{}, false, nil
	}
	i := len(o.versions) - 1
	return Object{VersionID: fmt.Sprint(i), ETag: o.etag(i)}, true, nil
}

func (o *memObjects) Download(_ context.Context, _, versionID, path string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	var i int
	if _, err := fmt.Sscan(versionID, &i); err != nil || i >= len(o.versions) {
		return errors.New("no such version")
	}
	return os.WriteFile(path, o.versions[i], 0o600)
}

func (o *memObjects) Upload(_ context.Context, _, path, ifMatch string) (Object, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	cur := ""
	if len(o.versions) > 0 {
		cur = o.etag(len(o.versions) - 1)
	}
	if ifMatch != cur {
		return Object{}, ErrConflict
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Object{}, err
	}
	o.versions = append(o.versions, b)
	o.uploads++
	i := len(o.versions) - 1
	return Object{VersionID: fmt.Sprint(i), ETag: o.etag(i)}, nil
}

// racingObjects publishes a competing version just before each upload.
type racingObjects struct{ *memObjects }

func (o *racingObjects) Upload(ctx context.Context, key, path, ifMatch string) (Object, error) {
	o.mu.Lock()
	o.versions = append(o.versions, nil)
	o.mu.Unlock()
	return o.memObjects.Upload(ctx, key, path, ifMatch)
}

type wakeCount struct{ n int }

func (w *wakeCount) Wake(context.Context, int64) error { w.n++; return nil }

func pipeline(t *testing.T) (*memSource, *memObjects, *Indexer, *Reader) {
	src := &memSource{st: model.State{Epoch: 1}, emails: map[string]model.Email{}, raw: map[string][]byte{}, now: time.Now()}
	objs := &memObjects{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ix := &Indexer{Source: src, Registrar: src, Originals: src, Objects: objs, Key: "index/primary.db", Dir: t.TempDir(), Log: log}
	clock := time.Now()
	rd := &Reader{Source: src, Objects: objs, Dir: t.TempDir(), Log: log, Now: func() time.Time { clock = clock.Add(time.Minute); return clock }}
	return src, objs, ix, rd
}

func match(t *testing.T, rd *Reader, q string) ([]string, string) {
	t.Helper()
	resp, err := rd.Serve(context.Background(), Request{Op: "match", Conditions: []Condition{{Field: "text", Value: q}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Error != "" {
		return nil, resp.Error
	}
	slices.Sort(resp.Matches[0])
	return resp.Matches[0], ""
}

func TestPipeline(t *testing.T) {
	ctx := context.Background()
	src, objs, ix, rd := pipeline(t)
	if _, e := match(t, rd, "x"); e != "notReady" {
		t.Errorf("before the first generation: %q", e)
	}
	src.add("Ma", "Budget", "the annual budget")
	src.add("Mb", "Lunch", "会议之后吃午饭")
	w := &wakeCount{}
	if woke, _ := Dispatch(ctx, src, w); !woke {
		t.Fatal("no generation yet: wake")
	}
	if err := ix.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if ids, _ := match(t, rd, "budget"); !slices.Equal(ids, []string{"Ma"}) {
		t.Errorf("budget %v", ids)
	}
	if woke, _ := Dispatch(ctx, src, w); woke {
		t.Error("registered generation is current: no wake-up")
	}
	src.touch()
	if woke, _ := Dispatch(ctx, src, w); woke {
		t.Error("keyword-only changes need no indexing")
	}

	// Destroy and create; a crash after publication is finished by the next
	// run without publishing again.
	src.destroy("Ma")
	src.add("Mc", "Budget v2", "revised budget")
	if woke, _ := Dispatch(ctx, src, w); !woke {
		t.Error("create/destroy wakes the indexer")
	}
	src.failReg = 1
	if err := ix.Run(ctx); err == nil {
		t.Fatal("injected registration crash")
	}
	uploads := objs.uploads
	if err := ix.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if objs.uploads != uploads {
		t.Errorf("recovery republished: %d uploads, want %d", objs.uploads, uploads)
	}
	if ids, _ := match(t, rd, "budget"); !slices.Equal(ids, []string{"Mc"}) {
		t.Errorf("after destroy/create %v", ids)
	}
	if ids, _ := match(t, rd, "会议"); !slices.Equal(ids, []string{"Mb"}) {
		t.Errorf("cjk %v", ids)
	}
	if err := ix.Run(ctx); err != nil || objs.uploads != uploads {
		t.Errorf("an idle run publishes nothing: %v %d", err, objs.uploads)
	}

	// Snippets come from the same generation.
	resp, _ := rd.Serve(ctx, Request{Op: "snippets", EmailIDs: []string{"Mc", "Ma"}, Terms: []string{"budget"}})
	if s := resp.Snippets["Mc"]; s.Subject == nil || *s.Subject != "<mark>Budget</mark> v2" || len(resp.Snippets) != 1 {
		t.Errorf("snippets %+v", resp.Snippets)
	}

	// A writer that loses the conditional PUT neither overwrites the
	// object nor registers its generation.
	src.add("Md", "x", "y")
	before, _, _ := src.SearchIndex(ctx)
	ix.Objects = &racingObjects{memObjects: objs}
	if err := ix.Run(ctx); !errors.Is(err, ErrConflict) {
		t.Errorf("lost race: %v", err)
	}
	if after, _, _ := src.SearchIndex(ctx); after != before {
		t.Errorf("lost race registered %+v", after)
	}
	ix.Objects = objs

	// Lag beyond MaxLag fails retryably.
	src.mu.Lock()
	src.now = time.Now().Add(-time.Hour)
	src.mu.Unlock()
	src.add("Me", "late", "late")
	rd.Now = func() time.Time { return time.Now().Add(time.Hour) }
	if _, e := match(t, rd, "late"); e != "lagging" {
		t.Errorf("lag: %q", e)
	}
}

func TestIndexerRebuildsOnEpochChange(t *testing.T) {
	ctx := context.Background()
	src, _, ix, rd := pipeline(t)
	src.add("Ma", "Budget", "x")
	if err := ix.Run(ctx); err != nil {
		t.Fatal(err)
	}
	src.mu.Lock()
	src.st.Epoch = 2
	delete(src.emails, "Ma")
	src.emails["Mz"] = model.Email{ID: "Mz", ThreadID: "Tz", Blob: model.BlobRef{Key: "Mz"}}
	src.raw["Mz"] = []byte("Subject: restored\r\n\r\nbody\r\n")
	src.mu.Unlock()
	if err := ix.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if ids, _ := match(t, rd, "budget"); len(ids) != 0 {
		t.Errorf("old epoch content survives: %v", ids)
	}
	if ids, _ := match(t, rd, "restored"); !slices.Equal(ids, []string{"Mz"}) {
		t.Errorf("rebuild %v", ids)
	}
}
