package search

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/zero11-0203/s3mail/go/internal/model"
)

// Source is the account data the pipeline reads, strongly consistent.
type Source interface {
	State(ctx context.Context) (model.State, error)
	Changes(ctx context.Context, after, upto int64) ([]model.Change, error)
	ListEmails(ctx context.Context) ([]model.Email, error)
	GetEmails(ctx context.Context, ids []string) (map[string]model.Email, error)
	SearchIndex(ctx context.Context) (model.SearchIndex, bool, error)
}

// Registrar registers a published generation with a guarded commit.
type Registrar interface {
	Commit(ctx context.Context, prev model.State, m model.Mutation) (model.State, error)
}

// Originals reads raw MIME by exact version.
type Originals interface {
	Fetch(ctx context.Context, ref model.BlobRef) ([]byte, error)
}

// Object is the current version of the index object.
type Object struct {
	VersionID, ETag string
}

// Objects stores index snapshots in the versioned index bucket.
type Objects interface {
	// Head returns the current version, or false when none exists.
	Head(ctx context.Context, key string) (Object, bool, error)
	// Download writes one exact version to path.
	Download(ctx context.Context, key, versionID, path string) error
	// Upload publishes path conditionally on the current ETag ("" means
	// the object must not exist) and returns the new version; a lost race
	// is ErrConflict.
	Upload(ctx context.Context, key, path, ifMatch string) (Object, error)
}

// Errors of the pipeline.
var (
	ErrConflict = errors.New("index object changed concurrently")
	ErrNotReady = errors.New("search index is not ready")
	ErrLagging  = errors.New("search index is too far behind")
)

// MaxLag is how old the oldest unindexed change may be before text
// queries fail retryably (docs/specs/search.md#index-pipeline).
const MaxLag = 15 * time.Minute

func newGeneration() string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Indexer is the single writer: it replays changes after the published
// cursor onto the current snapshot, publishes with a conditional PUT and
// then registers the generation.
type Indexer struct {
	Source    Source
	Registrar Registrar
	Originals Originals
	Objects   Objects
	Key       string
	Dir       string
	Log       *slog.Logger
	Now       func() time.Time
}

func (ix *Indexer) now() time.Time {
	if ix.Now != nil {
		return ix.Now()
	}
	return time.Now()
}

// Run brings the published index up to the current account state. It is
// idempotent: a crash between publication and registration is finished
// from the snapshot's embedded metadata on the next run.
func (ix *Indexer) Run(ctx context.Context) error {
	reg, registered, err := ix.Source.SearchIndex(ctx)
	if err != nil {
		return fmt.Errorf("read registration: %w", err)
	}
	head, exists, err := ix.Objects.Head(ctx, ix.Key)
	if err != nil {
		return fmt.Errorf("head index: %w", err)
	}
	st, err := ix.Source.State(ctx)
	if err != nil {
		return err
	}
	if registered && exists && head.VersionID == reg.VersionID {
		// The registered generation is the current object: skip the
		// download when nothing search-relevant happened since.
		if due, err := pending(ctx, ix.Source, reg, st); err != nil || !due {
			return err
		}
	}
	work := filepath.Join(ix.Dir, "work-"+newGeneration()+".db")
	defer func() { _ = os.Remove(work) }()

	var x *Index
	var meta Meta
	if exists {
		if err := ix.Objects.Download(ctx, ix.Key, head.VersionID, work); err != nil {
			return fmt.Errorf("download index: %w", err)
		}
		if err := Verify(work); err != nil {
			ix.Log.Warn("index snapshot unusable; rebuilding", "error", err)
			_ = os.Remove(work)
		} else if x, err = Open(work); err != nil {
			return err
		} else if meta, err = x.Meta(); err != nil {
			_ = x.Close()
			return err
		}
	}
	if x != nil && meta.Epoch != st.Epoch {
		// A restored account is reindexed from its live Emails.
		_ = x.Close()
		x = nil
		_ = os.Remove(work)
	}
	if x != nil && (!registered || reg.Generation != meta.Generation) && meta.Seq >= reg.Seq {
		// Published but not registered: finish the registration first.
		if err := ix.register(ctx, reg, registered, meta, head); err != nil {
			_ = x.Close()
			return err
		}
		reg, registered = model.SearchIndex{Generation: meta.Generation, Seq: meta.Seq}, true
	}
	if x == nil {
		if x, err = Open(work); err != nil {
			return err
		}
		meta = Meta{Epoch: st.Epoch, Schema: SchemaVersion}
	}
	defer func() { _ = x.Close() }()
	if meta.Seq >= st.Seq && meta.Generation != "" {
		return nil
	}

	var dirty []string
	if meta.Seq == 0 {
		all, err := ix.Source.ListEmails(ctx)
		if err != nil {
			return err
		}
		for _, e := range all {
			dirty = append(dirty, e.ID)
		}
	} else {
		changes, err := ix.Source.Changes(ctx, meta.Seq, st.Seq)
		if err != nil {
			return err
		}
		for _, ch := range changes {
			for _, id := range slices.Concat(ch.Created["Email"], ch.Destroyed["Email"]) {
				if !slices.Contains(dirty, id) {
					dirty = append(dirty, id)
				}
			}
		}
	}
	if len(dirty) == 0 && meta.Generation != "" {
		// Only keyword, mailbox or registration changes: the published
		// content is current. Its cursor stays put and the next replay
		// skips these changes again.
		return nil
	}
	docs, deleted, err := ix.docs(ctx, dirty)
	if err != nil {
		return err
	}
	next := Meta{Generation: newGeneration(), Seq: st.Seq, Epoch: st.Epoch, Schema: SchemaVersion}
	if err := x.Apply(docs, deleted, next); err != nil {
		return fmt.Errorf("apply: %w", err)
	}
	snap := filepath.Join(ix.Dir, "snap-"+next.Generation+".db")
	defer func() { _ = os.Remove(snap) }()
	if err := x.Snapshot(snap); err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	ifMatch := ""
	if exists {
		ifMatch = head.ETag
	}
	obj, err := ix.Objects.Upload(ctx, ix.Key, snap, ifMatch)
	if err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	if err := ix.register(ctx, reg, registered, next, obj); err != nil {
		return err
	}
	ix.Log.Info("index published", "generation", next.Generation, "seq", next.Seq, "docs", len(docs), "deleted", len(deleted))
	return nil
}

// docs reads the live Emails among ids; the rest are removed.
func (ix *Indexer) docs(ctx context.Context, ids []string) ([]Doc, []string, error) {
	live := map[string]model.Email{}
	for chunk := range slices.Chunk(ids, 100) {
		found, err := ix.Source.GetEmails(ctx, chunk)
		if err != nil {
			return nil, nil, err
		}
		for k, v := range found {
			live[k] = v
		}
	}
	var deleted []string
	for _, id := range ids {
		if _, ok := live[id]; !ok {
			deleted = append(deleted, id)
		}
	}
	docs := make([]Doc, 0, len(live))
	var mu sync.Mutex
	var firstErr error
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, e := range live {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			raw, err := ix.Originals.Fetch(ctx, e.Blob)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				firstErr = errors.Join(firstErr, fmt.Errorf("fetch %s: %w", e.ID, err))
				return
			}
			docs = append(docs, Extract(e.ID, e.ThreadID, e.ReceivedAt, raw))
		}()
	}
	wg.Wait()
	return docs, deleted, firstErr
}

func (ix *Indexer) register(ctx context.Context, prev model.SearchIndex, registered bool, m Meta, obj Object) error {
	prevGen := ""
	if registered {
		prevGen = prev.Generation
	}
	for range 5 {
		st, err := ix.Source.State(ctx)
		if err != nil {
			return err
		}
		_, err = ix.Registrar.Commit(ctx, st, model.Mutation{Search: &model.SearchRegistration{Prev: prevGen, Next: model.SearchIndex{
			Generation: m.Generation, Seq: m.Seq, Epoch: m.Epoch, Key: ix.Key, VersionID: obj.VersionID, ETag: obj.ETag, PublishedAt: ix.now(),
		}}})
		if errors.Is(err, model.ErrStateChanged) {
			continue
		}
		if err != nil {
			return fmt.Errorf("register generation: %w", err)
		}
		return nil
	}
	return errors.New("register generation: account busy")
}

// Request is the search function's internal protocol.
type Request struct {
	// Op is "match" or "snippets".
	Op         string      `json:"op"`
	Conditions []Condition `json:"conditions,omitempty"`
	EmailIDs   []string    `json:"emailIds,omitempty"`
	Terms      []string    `json:"terms,omitempty"`
}

// Response answers a Request from one generation.
type Response struct {
	Generation string             `json:"generation"`
	Matches    [][]string         `json:"matches,omitempty"`
	Snippets   map[string]Snippet `json:"snippets,omitempty"`
	// Error is "notReady" or "lagging" for retryable unavailability.
	Error string `json:"error,omitempty"`
}

// Reader serves queries from the registered generation, downloading each
// new generation to a new file and swapping connections atomically.
type Reader struct {
	Source  Source
	Objects Objects
	Dir     string
	Log     *slog.Logger
	Now     func() time.Time

	mu        sync.Mutex
	gen       string
	path      string
	index     *Index
	checkedAt time.Time
}

// refreshEvery bounds how long a cached generation is used unchecked.
const refreshEvery = 15 * time.Second

func (r *Reader) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Serve answers one request; every answer comes from one generation.
func (r *Reader) Serve(ctx context.Context, req Request) (Response, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.index == nil || r.now().Sub(r.checkedAt) >= refreshEvery {
		if err := r.refresh(ctx); err != nil {
			switch {
			case errors.Is(err, ErrNotReady):
				return Response{Error: "notReady"}, nil
			case errors.Is(err, ErrLagging):
				return Response{Error: "lagging"}, nil
			}
			return Response{}, err
		}
	}
	resp := Response{Generation: r.gen}
	switch req.Op {
	case "match":
		for _, c := range req.Conditions {
			ids, err := r.index.Match(c)
			if err != nil {
				return Response{}, err
			}
			resp.Matches = append(resp.Matches, ids)
		}
	case "snippets":
		stored, err := r.index.Stored(req.EmailIDs)
		if err != nil {
			return Response{}, err
		}
		resp.Snippets = map[string]Snippet{}
		for id, v := range stored {
			resp.Snippets[id] = Highlight(v[0], v[1], req.Terms)
		}
	default:
		return Response{}, fmt.Errorf("unknown op %q", req.Op)
	}
	return resp, nil
}

func (r *Reader) refresh(ctx context.Context) error {
	reg, ok, err := r.Source.SearchIndex(ctx)
	if err != nil {
		return err
	}
	st, err := r.Source.State(ctx)
	if err != nil {
		return err
	}
	if !ok || reg.Epoch != st.Epoch {
		return ErrNotReady
	}
	if err := r.checkLag(ctx, reg, st); err != nil {
		return err
	}
	if reg.Generation != r.gen {
		path := filepath.Join(r.Dir, "index-"+reg.Generation+".db")
		if err := r.Objects.Download(ctx, reg.Key, reg.VersionID, path); err != nil {
			return fmt.Errorf("download generation: %w", err)
		}
		if err := Verify(path); err != nil {
			_ = os.Remove(path)
			return err
		}
		x, err := OpenReadOnly(path)
		if err != nil {
			return err
		}
		if m, err := x.Meta(); err != nil || m.Generation != reg.Generation {
			_ = x.Close()
			_ = os.Remove(path)
			return fmt.Errorf("downloaded snapshot is not generation %s", reg.Generation)
		}
		if r.index != nil {
			_ = r.index.Close()
			_ = os.Remove(r.path)
		}
		r.gen, r.path, r.index = reg.Generation, path, x
	}
	r.checkedAt = r.now()
	return nil
}

// checkLag fails when an unindexed search-relevant change is older than
// MaxLag.
func (r *Reader) checkLag(ctx context.Context, reg model.SearchIndex, st model.State) error {
	if st.Seq <= reg.Seq {
		return nil
	}
	changes, err := r.Source.Changes(ctx, reg.Seq, st.Seq)
	if err != nil {
		return err
	}
	for _, ch := range changes {
		if len(ch.Created["Email"])+len(ch.Destroyed["Email"]) == 0 || ch.At.IsZero() {
			continue
		}
		if r.now().Sub(ch.At) > MaxLag {
			return ErrLagging
		}
		break
	}
	return nil
}

// Waker sends one wake-up to the index queue.
type Waker interface {
	Wake(ctx context.Context, seq int64) error
}

// Dispatch wakes the indexer when Emails were created or destroyed after
// the registered generation. Wake-ups carry no ordering: the indexer
// replays from its own cursor.
func Dispatch(ctx context.Context, src Source, w Waker) (bool, error) {
	reg, ok, err := src.SearchIndex(ctx)
	if err != nil {
		return false, err
	}
	st, err := src.State(ctx)
	if err != nil {
		return false, err
	}
	if ok {
		if due, err := pending(ctx, src, reg, st); err != nil || !due {
			return false, err
		}
	}
	return true, w.Wake(ctx, st.Seq)
}

// pending reports whether the registered generation misses Emails created
// or destroyed since, or belongs to an earlier epoch.
func pending(ctx context.Context, src Source, reg model.SearchIndex, st model.State) (bool, error) {
	if reg.Epoch != st.Epoch {
		return true, nil
	}
	if reg.Seq >= st.Seq {
		return false, nil
	}
	changes, err := src.Changes(ctx, reg.Seq, st.Seq)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(changes, func(ch model.Change) bool {
		return len(ch.Created["Email"])+len(ch.Destroyed["Email"]) > 0
	}), nil
}
