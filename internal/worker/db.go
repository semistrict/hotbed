package worker

import (
	"context"
	"database/sql"
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite"
)

// db is the worker's own state, so a restarted worker picks up its bases,
// workspaces and running commands where it left them. Each record is one
// message, stored as protobuf JSON: the worker reads them all at start and
// writes each one whenever it changes.
type db struct {
	sql *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS records (
	kind TEXT NOT NULL,   -- base, workspace, command
	id   TEXT NOT NULL,
	doc  TEXT NOT NULL,
	PRIMARY KEY (kind, id)
) STRICT;
CREATE TABLE IF NOT EXISTS outbox (
	seq  INTEGER PRIMARY KEY AUTOINCREMENT,
	kind TEXT NOT NULL,   -- finished, event
	doc  TEXT NOT NULL
) STRICT;
`

func openDB(ctx context.Context, path string) (*db, error) {
	s, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	s.SetMaxOpenConns(1)
	if _, err := s.ExecContext(ctx, schema); err != nil {
		s.Close()
		return nil, fmt.Errorf("create worker schema: %w", err)
	}
	return &db{sql: s}, nil
}

func (d *db) close() error { return d.sql.Close() }

func (d *db) put(kind, id string, m proto.Message) error {
	b, err := protojson.Marshal(m)
	if err != nil {
		return err
	}
	_, err = d.sql.Exec(`INSERT INTO records (kind, id, doc) VALUES (?, ?, ?)
		ON CONFLICT (kind, id) DO UPDATE SET doc = excluded.doc`, kind, id, string(b))
	return err
}

func (d *db) delete(kind, id string) error {
	_, err := d.sql.Exec(`DELETE FROM records WHERE kind = ? AND id = ?`, kind, id)
	return err
}

// all decodes every record of a kind into a new message and hands it to fn.
func (d *db) all(kind string, newMsg func() proto.Message, fn func(proto.Message)) error {
	rows, err := d.sql.Query(`SELECT doc FROM records WHERE kind = ?`, kind)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return err
		}
		m := newMsg()
		if err := decode([]byte(doc), m); err != nil {
			return fmt.Errorf("decode %s record: %w", kind, err)
		}
		fn(m)
	}
	return rows.Err()
}

// enqueue adds an item for the controller and returns its sequence number.
func (d *db) enqueue(kind string, m proto.Message) (int64, error) {
	b, err := protojson.Marshal(m)
	if err != nil {
		return 0, err
	}
	res, err := d.sql.Exec(`INSERT INTO outbox (kind, doc) VALUES (?, ?)`, kind, string(b))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

type outboxItem struct {
	seq  int64
	kind string
	doc  []byte
}

// pending returns up to limit items the controller has not acknowledged.
func (d *db) pending(limit int) ([]outboxItem, error) {
	rows, err := d.sql.Query(`SELECT seq, kind, doc FROM outbox ORDER BY seq LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []outboxItem
	for rows.Next() {
		var it outboxItem
		var doc string
		if err := rows.Scan(&it.seq, &it.kind, &doc); err != nil {
			return nil, err
		}
		it.doc = []byte(doc)
		out = append(out, it)
	}
	return out, rows.Err()
}

// ack drops every item up to seq.
func (d *db) ack(seq int64) error {
	_, err := d.sql.Exec(`DELETE FROM outbox WHERE seq <= ?`, seq)
	return err
}

// decode reads a stored record. Fields a newer hotbed dropped are ignored,
// so records written before an upgrade still load.
func decode(doc []byte, m proto.Message) error {
	return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(doc, m)
}
