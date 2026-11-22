package worker

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// db is the worker's own state, the truth about its bases, workspaces and
// commands, so a restarted worker picks them up where it left them. Each
// record is one message, stored as protobuf JSON: the worker reads them all
// at start and writes each one whenever it changes, in the same transaction
// as the event that says what changed. The events are numbered in order, and
// the controller folds them into its view of the node; the records are
// those events folded, which is what a checkpoint sends.
type db struct {
	sql   *sql.DB
	epoch string
}

const schema = `
CREATE TABLE IF NOT EXISTS records (
	kind TEXT NOT NULL,   -- base, workspace, command
	id   TEXT NOT NULL,
	doc  TEXT NOT NULL,
	PRIMARY KEY (kind, id)
) STRICT;
-- Workers before the event stream queued what the controller had not
-- acknowledged here; one that still has items delivers them once.
CREATE TABLE IF NOT EXISTS outbox (
	seq  INTEGER PRIMARY KEY AUTOINCREMENT,
	kind TEXT NOT NULL,   -- finished, event, session
	doc  TEXT NOT NULL
) STRICT;
-- AUTOINCREMENT: a sequence number is never used twice, even once pruned.
CREATE TABLE IF NOT EXISTS events (
	seq INTEGER PRIMARY KEY AUTOINCREMENT,
	at  INTEGER NOT NULL, -- unix ms
	doc TEXT NOT NULL     -- an Event, without its seq
) STRICT;
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
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
	// The epoch names this database: a worker whose database is lost starts
	// a new one, so the controller knows its sequence started again.
	b := make([]byte, 16)
	rand.Read(b)
	if _, err := s.ExecContext(ctx, `INSERT OR IGNORE INTO meta (key, value) VALUES ('epoch', ?)`, hex.EncodeToString(b)); err != nil {
		s.Close()
		return nil, err
	}
	d := &db{sql: s}
	if err := s.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'epoch'`).Scan(&d.epoch); err != nil {
		s.Close()
		return nil, err
	}
	return d, nil
}

func (d *db) close() error { return d.sql.Close() }

// commit writes a record's new state (or deletes it, when rec is nil) and
// the event that says so, in one transaction, and numbers the event.
func (d *db) commit(kind, id string, rec proto.Message, ev *pb.Event) error {
	evDoc, err := protojson.Marshal(ev)
	if err != nil {
		return err
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if rec == nil {
		_, err = tx.Exec(`DELETE FROM records WHERE kind = ? AND id = ?`, kind, id)
	} else {
		var doc []byte
		if doc, err = protojson.Marshal(rec); err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO records (kind, id, doc) VALUES (?, ?, ?)
			ON CONFLICT (kind, id) DO UPDATE SET doc = excluded.doc`, kind, id, string(doc))
	}
	if err != nil {
		return err
	}
	res, err := tx.Exec(`INSERT INTO events (at, doc) VALUES (?, ?)`, ev.At, string(evDoc))
	if err != nil {
		return err
	}
	if ev.Seq, err = res.LastInsertId(); err != nil {
		return err
	}
	return tx.Commit()
}

// all decodes every record of a kind into a new message and hands it to fn.
func (d *db) all(kind string, newMsg func() proto.Message, fn func(proto.Message)) error {
	return all(d.sql, kind, newMsg, fn)
}

type querier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

func all(q querier, kind string, newMsg func() proto.Message, fn func(proto.Message)) error {
	rows, err := q.Query(`SELECT doc FROM records WHERE kind = ?`, kind)
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

// events returns up to limit events after seq, oldest first.
func (d *db) events(after int64, limit int) ([]*pb.Event, error) {
	rows, err := d.sql.Query(`SELECT seq, doc FROM events WHERE seq > ? ORDER BY seq LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*pb.Event{}
	for rows.Next() {
		var seq int64
		var doc string
		if err := rows.Scan(&seq, &doc); err != nil {
			return nil, err
		}
		ev := &pb.Event{}
		if err := decode([]byte(doc), ev); err != nil {
			return nil, fmt.Errorf("decode event %d: %w", seq, err)
		}
		ev.Seq = seq
		out = append(out, ev)
	}
	return out, rows.Err()
}

// eventRange is the oldest event kept and the newest ever numbered; oldest
// is newest+1 when none is kept.
func (d *db) eventRange() (oldest, newest int64, err error) {
	err = d.sql.QueryRow(`SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name = 'events'), 0)`).Scan(&newest)
	if err != nil {
		return 0, 0, err
	}
	err = d.sql.QueryRow(`SELECT COALESCE(MIN(seq), ?) FROM events`, newest+1).Scan(&oldest)
	return oldest, newest, err
}

// checkpoint reads every record and the last event they include, in one
// transaction.
func (d *db) checkpoint() (*pb.Checkpoint, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	cp := &pb.Checkpoint{}
	if err := tx.QueryRow(`SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name = 'events'), 0)`).Scan(&cp.Seq); err != nil {
		return nil, err
	}
	if err := all(tx, "base", func() proto.Message { return &pb.Base{} }, func(m proto.Message) {
		cp.Bases = append(cp.Bases, m.(*pb.Base))
	}); err != nil {
		return nil, err
	}
	if err := all(tx, "workspace", func() proto.Message { return &pb.WorkspaceRecord{} }, func(m proto.Message) {
		cp.Workspaces = append(cp.Workspaces, m.(*pb.WorkspaceRecord).Workspace)
	}); err != nil {
		return nil, err
	}
	if err := all(tx, "command", func() proto.Message { return &pb.CommandRecord{} }, func(m proto.Message) {
		cp.Commands = append(cp.Commands, m.(*pb.CommandRecord).Command)
	}); err != nil {
		return nil, err
	}
	return cp, nil
}

// prune drops the events the controller has applied that are older than
// before (unix ms), and, however far behind the controller is, all but the
// newest keep: a controller that falls behind those gets a checkpoint.
func (d *db) prune(applied, before int64, keep int) error {
	_, err := d.sql.Exec(`DELETE FROM events WHERE (seq <= ? AND at < ?) OR seq <= (SELECT MAX(seq) FROM events) - ?`, applied, before, keep)
	return err
}

type outboxItem struct {
	seq  int64
	kind string
	doc  []byte
}

// pending returns up to limit items of the old outbox not yet delivered.
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

// ack drops every outbox item up to seq.
func (d *db) ack(seq int64) error {
	_, err := d.sql.Exec(`DELETE FROM outbox WHERE seq <= ?`, seq)
	return err
}

// decode reads a stored record. Fields a newer hotbed dropped are ignored,
// so records written before an upgrade still load.
func decode(doc []byte, m proto.Message) error {
	return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(doc, m)
}

// newEpoch names the database anew, for when it is an older copy of itself.
func (d *db) newEpoch() error {
	b := make([]byte, 16)
	rand.Read(b)
	epoch := hex.EncodeToString(b)
	if _, err := d.sql.Exec(`UPDATE meta SET value = ? WHERE key = 'epoch'`, epoch); err != nil {
		return err
	}
	d.epoch = epoch
	return nil
}

// eventAt is the time of the event numbered seq, or 0 when it is not kept.
func (d *db) eventAt(seq int64) (int64, error) {
	var at int64
	err := d.sql.QueryRow(`SELECT COALESCE((SELECT at FROM events WHERE seq = ?), 0)`, seq).Scan(&at)
	return at, err
}
