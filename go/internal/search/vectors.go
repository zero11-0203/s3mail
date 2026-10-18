package search

import (
	"container/heap"
	"encoding/binary"
	"errors"
	"math"
)

// docs_vec holds one unit-length float32 vector per indexed Email, keyed by
// its docs row, from the model named in meta embed_model. The table is
// additive: a snapshot without it has no vectors, and changing the model
// clears them so they are rebuilt from the stored text.

// Pending is an indexed Email still without a vector.
type Pending struct {
	Row  int64
	Text string
}

// Hit is one semantic match.
type Hit struct {
	EmailID string  `json:"emailId"`
	Score   float32 `json:"score"`
}

func encodeVec(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(x))
	}
	return b
}

func (x *Index) hasVectors() (bool, error) {
	st, _, err := x.db.Prepare(`SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'docs_vec'`)
	if err != nil {
		return false, err
	}
	defer func() { _ = st.Close() }()
	found := st.Step()
	return found, st.Err()
}

func (x *Index) metaValue(key string) (string, error) {
	st, _, err := x.db.Prepare(`SELECT value FROM meta WHERE key = ?`)
	if err != nil {
		return "", err
	}
	defer func() { _ = st.Close() }()
	if err := st.BindText(1, key); err != nil {
		return "", err
	}
	v := ""
	if st.Step() {
		v = st.ColumnText(0)
	}
	return v, st.Err()
}

// UseModel prepares the vectors for model: a different model's vectors are
// cleared, since vectors of two models cannot be compared.
func (x *Index) UseModel(model string) (err error) {
	current, err := x.metaValue("embed_model")
	if err != nil || current == model {
		return err
	}
	tx := x.db.Begin()
	defer tx.End(&err)
	if err := x.exec(`DELETE FROM docs_vec`); err != nil {
		return err
	}
	return x.exec(`INSERT INTO meta(key, value) VALUES ('embed_model', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, model)
}

// MissingVectors returns up to limit indexed Emails without a vector,
// newest first, with the text to embed.
func (x *Index) MissingVectors(limit int) ([]Pending, error) {
	st, _, err := x.db.Prepare(`SELECT d.id, d.subject, d.body FROM docs d LEFT JOIN docs_vec v ON v.id = d.id WHERE v.id IS NULL ORDER BY d.received_at DESC LIMIT ?`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close() }()
	if err := st.BindInt(1, limit); err != nil {
		return nil, err
	}
	var out []Pending
	for st.Step() {
		out = append(out, Pending{Row: st.ColumnInt64(0), Text: EmbedText(st.ColumnText(1), st.ColumnText(2))})
	}
	return out, st.Err()
}

// PutVectors stores vectors by docs row in one transaction.
func (x *Index) PutVectors(vecs map[int64][]float32) (err error) {
	tx := x.db.Begin()
	defer tx.End(&err)
	st, _, err := x.db.Prepare(`INSERT INTO docs_vec(id, vec) VALUES (?, ?) ON CONFLICT(id) DO UPDATE SET vec = excluded.vec`)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	for row, v := range vecs {
		if err := errors.Join(st.Reset(), st.BindInt64(1, row), st.BindBlob(2, encodeVec(v))); err != nil {
			return err
		}
		if st.Step(); st.Err() != nil {
			return st.Err()
		}
	}
	return nil
}

// Nearest returns the limit Emails whose vectors are closest to q (unit
// length) by cosine similarity, best first. It scans every vector, which
// is fast at personal-mailbox size; a vector of another length is skipped.
func (x *Index) Nearest(q []float32, limit int) ([]Hit, error) {
	if ok, err := x.hasVectors(); err != nil || !ok || limit <= 0 {
		return []Hit{}, err
	}
	st, _, err := x.db.Prepare(`SELECT d.email_id, v.vec FROM docs_vec v JOIN docs d ON d.id = v.id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close() }()
	top := &hits{}
	for st.Step() {
		b := st.ColumnRawBlob(1)
		if len(b) != 4*len(q) {
			continue
		}
		var dot float32
		for i := range q {
			dot += q[i] * math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
		}
		if top.Len() < limit {
			heap.Push(top, Hit{EmailID: st.ColumnText(0), Score: dot})
		} else if dot > (*top)[0].Score {
			(*top)[0] = Hit{EmailID: st.ColumnText(0), Score: dot}
			heap.Fix(top, 0)
		}
	}
	if err := st.Err(); err != nil {
		return nil, err
	}
	out := make([]Hit, top.Len())
	for i := len(out) - 1; i >= 0; i-- {
		out[i] = heap.Pop(top).(Hit)
	}
	return out, nil
}

// hits is a min-heap on score, holding the best matches so far.
type hits []Hit

func (h hits) Len() int           { return len(h) }
func (h hits) Less(i, j int) bool { return h[i].Score < h[j].Score }
func (h hits) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *hits) Push(v any)        { *h = append(*h, v.(Hit)) }
func (h *hits) Pop() any {
	old := *h
	v := old[len(old)-1]
	*h = old[:len(old)-1]
	return v
}
