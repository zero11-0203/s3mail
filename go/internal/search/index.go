package search

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/ext/fts5"
)

// SchemaVersion changes whenever the schema or text pipeline does; a
// snapshot with another version is rebuilt, never queried.
const SchemaVersion = 1

const schema = `
CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS docs(
	id INTEGER PRIMARY KEY,
	email_id TEXT NOT NULL UNIQUE,
	thread_id TEXT NOT NULL,
	received_at TEXT NOT NULL,
	has_attachment INTEGER NOT NULL,
	subject TEXT NOT NULL,
	body TEXT NOT NULL
);
CREATE VIRTUAL TABLE IF NOT EXISTS docs_fts USING fts5(
	from_t, to_t, cc_t, bcc_t, subject, body, attachments,
	content='', contentless_delete=1,
	tokenize='unicode61 remove_diacritics 2'
);`

// Field names the JMAP filter conditions map to.
var fieldColumns = map[string][]string{
	"text":    {"from_t", "to_t", "cc_t", "bcc_t", "subject", "body", "attachments"},
	"from":    {"from_t"},
	"to":      {"to_t"},
	"cc":      {"cc_t"},
	"bcc":     {"bcc_t"},
	"subject": {"subject"},
	"body":    {"body"},
}

// Meta is the source position a snapshot embeds
// (docs/specs/search.md#index-schema-sketch).
type Meta struct {
	Generation string
	Seq, Epoch int64
	Schema     int
}

// Index is an open index database.
type Index struct{ db *sqlite3.Conn }

// connect opens a connection with the FTS5 extension registered.
func connect(uri string, flags sqlite3.OpenFlag) (*sqlite3.Conn, error) {
	db, err := sqlite3.OpenFlags(uri, flags|sqlite3.OPEN_URI)
	if err != nil {
		return nil, err
	}
	if err := fts5.Register(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// Open opens or creates the index at path for writing.
func Open(path string) (*Index, error) {
	db, err := connect("file:"+path, sqlite3.OPEN_READWRITE|sqlite3.OPEN_CREATE)
	if err != nil {
		return nil, err
	}
	if err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Index{db: db}, nil
}

// OpenReadOnly opens a downloaded snapshot for queries.
func OpenReadOnly(path string) (*Index, error) {
	db, err := connect("file:"+path+"?immutable=1", sqlite3.OPEN_READONLY)
	if err != nil {
		return nil, err
	}
	return &Index{db: db}, nil
}

// Close closes the database.
func (x *Index) Close() error { return x.db.Close() }

// Meta reads the embedded source position; a new index has the zero Meta.
func (x *Index) Meta() (Meta, error) {
	var m Meta
	st, _, err := x.db.Prepare(`SELECT key, value FROM meta`)
	if err != nil {
		return m, err
	}
	defer func() { _ = st.Close() }()
	for st.Step() {
		v := st.ColumnText(1)
		switch st.ColumnText(0) {
		case "generation":
			m.Generation = v
		case "seq":
			m.Seq, _ = strconv.ParseInt(v, 10, 64)
		case "epoch":
			m.Epoch, _ = strconv.ParseInt(v, 10, 64)
		case "schema":
			m.Schema, _ = strconv.Atoi(v)
		}
	}
	return m, st.Err()
}

func (x *Index) exec(sql string, args ...any) error {
	st, _, err := x.db.Prepare(sql)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	for i, a := range args {
		switch v := a.(type) {
		case string:
			err = st.BindText(i+1, v)
		case int64:
			err = st.BindInt64(i+1, v)
		case bool:
			err = st.BindBool(i+1, v)
		default:
			err = fmt.Errorf("unsupported argument %T", a)
		}
		if err != nil {
			return err
		}
	}
	return st.Exec()
}

// Apply upserts docs and removes deleted ids, then records m, in one
// transaction.
func (x *Index) Apply(docs []Doc, deleted []string, m Meta) (err error) {
	tx := x.db.Begin()
	defer tx.End(&err)
	for _, id := range deleted {
		if err := x.remove(id); err != nil {
			return err
		}
	}
	for _, d := range docs {
		if err := x.remove(d.EmailID); err != nil {
			return err
		}
		if err := x.exec(`INSERT INTO docs(email_id, thread_id, received_at, has_attachment, subject, body) VALUES (?, ?, ?, ?, ?, ?)`,
			d.EmailID, d.ThreadID, d.ReceivedAt.UTC().Format(time.RFC3339), d.HasAttachment, d.Subject, capBytes(d.Body, maxStoredBody)); err != nil {
			return err
		}
		rowid := x.db.LastInsertRowID()
		if err := x.exec(`INSERT INTO docs_fts(rowid, from_t, to_t, cc_t, bcc_t, subject, body, attachments) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			rowid, IndexText(d.From), IndexText(d.To), IndexText(d.Cc), IndexText(d.Bcc), IndexText(d.Subject), IndexText(d.Body), IndexText(d.AttachmentNames)); err != nil {
			return err
		}
	}
	for k, v := range map[string]string{
		"generation": m.Generation, "seq": strconv.FormatInt(m.Seq, 10),
		"epoch": strconv.FormatInt(m.Epoch, 10), "schema": strconv.Itoa(m.Schema),
	} {
		if err := x.exec(`INSERT INTO meta(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
			return err
		}
	}
	return nil
}

func (x *Index) remove(emailID string) error {
	st, _, err := x.db.Prepare(`SELECT id FROM docs WHERE email_id = ?`)
	if err != nil {
		return err
	}
	if err := st.BindText(1, emailID); err != nil {
		_ = st.Close()
		return err
	}
	var rowid int64
	found := st.Step()
	if found {
		rowid = st.ColumnInt64(0)
	}
	if err := errors.Join(st.Err(), st.Close()); err != nil || !found {
		return err
	}
	if err := x.exec(`DELETE FROM docs_fts WHERE rowid = ?`, rowid); err != nil {
		return err
	}
	return x.exec(`DELETE FROM docs WHERE id = ?`, rowid)
}

// Snapshot writes a closed, consistent single-file copy to path with the
// backup API and checks its integrity.
func (x *Index) Snapshot(path string) error {
	if err := x.db.Backup("main", "file:"+path); err != nil {
		return err
	}
	return Verify(path)
}

// Verify checks the schema version and runs SQLite's and FTS5's integrity
// checks. FTS5's check is issued as a write command, so the file is opened
// read-write; it changes nothing.
func Verify(path string) error {
	db, err := connect("file:"+path, sqlite3.OPEN_READWRITE)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	m, err := (&Index{db: db}).Meta()
	if err != nil {
		return err
	}
	if m.Schema != SchemaVersion {
		return fmt.Errorf("index schema %d, want %d", m.Schema, SchemaVersion)
	}
	if err := db.Exec(`INSERT INTO docs_fts(docs_fts, rank) VALUES ('integrity-check', 0)`); err != nil {
		return fmt.Errorf("fts integrity: %w", err)
	}
	st, _, err := db.Prepare(`PRAGMA integrity_check`)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	var problems []string
	for st.Step() {
		if r := st.ColumnText(0); r != "ok" {
			problems = append(problems, r)
		}
	}
	if err := st.Err(); err != nil {
		return err
	}
	if len(problems) > 0 {
		return fmt.Errorf("index integrity: %s", strings.Join(problems, "; "))
	}
	return nil
}

// Condition is one index-backed JMAP filter condition.
type Condition struct {
	// Field is text, from, to, cc, bcc, subject, body or hasAttachment.
	Field string `json:"field"`
	Value string `json:"value,omitempty"`
}

// Match returns the email ids matching c.
func (x *Index) Match(c Condition) ([]string, error) {
	var st *sqlite3.Stmt
	var err error
	switch c.Field {
	case "hasAttachment":
		want := c.Value != "false"
		st, _, err = x.db.Prepare(`SELECT email_id FROM docs WHERE has_attachment = ?`)
		if err == nil {
			err = st.BindBool(1, want)
		}
	default:
		cols, ok := fieldColumns[c.Field]
		if !ok {
			return nil, fmt.Errorf("unknown search field %q", c.Field)
		}
		expr := MatchExpr(c.Value, cols)
		if expr == "" {
			return []string{}, nil
		}
		st, _, err = x.db.Prepare(`SELECT d.email_id FROM docs_fts f JOIN docs d ON d.id = f.rowid WHERE docs_fts MATCH ?`)
		if err == nil {
			err = st.BindText(1, expr)
		}
	}
	if err != nil {
		if st != nil {
			_ = st.Close()
		}
		return nil, err
	}
	defer func() { _ = st.Close() }()
	ids := []string{}
	for st.Step() {
		ids = append(ids, st.ColumnText(0))
	}
	return ids, st.Err()
}

// Stored returns the subject and stored body text of the given Emails.
func (x *Index) Stored(ids []string) (map[string][2]string, error) {
	st, _, err := x.db.Prepare(`SELECT subject, body FROM docs WHERE email_id = ?`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close() }()
	out := map[string][2]string{}
	for _, id := range ids {
		if err := errors.Join(st.Reset(), st.BindText(1, id)); err != nil {
			return nil, err
		}
		if st.Step() {
			out[id] = [2]string{st.ColumnText(0), st.ColumnText(1)}
		}
		if err := st.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}
