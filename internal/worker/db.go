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
	workerdb "github.com/semistrict/hotbed/internal/worker/db/gen"
	workermigrations "github.com/semistrict/hotbed/internal/worker/db/migrations"
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
	q     *workerdb.Queries
	epoch string
}

func openDB(ctx context.Context, path string) (*db, error) {
	s, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, err
	}
	s.SetMaxOpenConns(1)
	if err := workermigrations.Migrate(ctx, s); err != nil {
		s.Close()
		return nil, fmt.Errorf("migrate worker database: %w", err)
	}
	q := workerdb.New(s)
	// The epoch names this database: a worker whose database is lost starts
	// a new one, so the controller knows its sequence started again.
	b := make([]byte, 16)
	rand.Read(b)
	if err := q.InsertMetaIgnore(ctx, workerdb.InsertMetaIgnoreParams{Key: "epoch", Value: hex.EncodeToString(b)}); err != nil {
		s.Close()
		return nil, err
	}
	d := &db{sql: s, q: q}
	if d.epoch, err = q.GetMeta(ctx, "epoch"); err != nil {
		s.Close()
		return nil, err
	}
	return d, nil
}

func (d *db) close() error { return d.sql.Close() }

// commit writes a record's new state (or deletes it, when rec is nil) and
// the event that says so, in one transaction, and numbers the event.
func (d *db) commit(kind, id string, rec proto.Message, ev *pb.Event) error {
	ctx := context.Background()
	evDoc, err := protojson.Marshal(ev)
	if err != nil {
		return err
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := d.q.WithTx(tx)
	if rec == nil {
		err = q.DeleteRecord(ctx, workerdb.DeleteRecordParams{Kind: kind, ID: id})
	} else {
		var doc []byte
		if doc, err = protojson.Marshal(rec); err != nil {
			return err
		}
		err = q.UpsertRecord(ctx, workerdb.UpsertRecordParams{Kind: kind, ID: id, Doc: string(doc)})
	}
	if err != nil {
		return err
	}
	res, err := q.InsertEvent(ctx, workerdb.InsertEventParams{At: ev.At, Doc: string(evDoc)})
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
	return allRecords(context.Background(), d.q, kind, newMsg, fn)
}

func allRecords(ctx context.Context, q *workerdb.Queries, kind string, newMsg func() proto.Message, fn func(proto.Message)) error {
	docs, err := q.ListRecordsByKind(ctx, kind)
	if err != nil {
		return err
	}
	for _, doc := range docs {
		m := newMsg()
		if err := decode([]byte(doc), m); err != nil {
			return fmt.Errorf("decode %s record: %w", kind, err)
		}
		fn(m)
	}
	return nil
}

// events returns up to limit events after seq, oldest first.
func (d *db) events(after int64, limit int) ([]*pb.Event, error) {
	rows, err := d.q.ListEventsAfter(context.Background(), workerdb.ListEventsAfterParams{Seq: after, Limit: int64(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]*pb.Event, 0, len(rows))
	for _, row := range rows {
		ev := &pb.Event{}
		if err := decode([]byte(row.Doc), ev); err != nil {
			return nil, fmt.Errorf("decode event %d: %w", row.Seq, err)
		}
		ev.Seq = row.Seq
		out = append(out, ev)
	}
	return out, nil
}

// eventRange is the oldest event kept and the newest ever numbered; oldest
// is newest+1 when none is kept.
func (d *db) eventRange() (oldest, newest int64, err error) {
	// sqlite_sequence is a SQLite internal table that sqlc does not model.
	err = d.sql.QueryRow(`SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name = 'events'), 0)`).Scan(&newest)
	if err != nil {
		return 0, 0, err
	}
	oldest, err = d.q.GetOldestEventSeq(context.Background(), newest+1)
	return oldest, newest, err
}

// checkpoint reads every record and the last event they include, in one
// transaction.
func (d *db) checkpoint() (*pb.Checkpoint, error) {
	ctx := context.Background()
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	q := d.q.WithTx(tx)
	cp := &pb.Checkpoint{}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name = 'events'), 0)`).Scan(&cp.Seq); err != nil {
		return nil, err
	}
	if err := allRecords(ctx, q, "base", func() proto.Message { return &pb.Base{} }, func(m proto.Message) {
		cp.Bases = append(cp.Bases, m.(*pb.Base))
	}); err != nil {
		return nil, err
	}
	if err := allRecords(ctx, q, "workspace", func() proto.Message { return &pb.WorkspaceRecord{} }, func(m proto.Message) {
		cp.Workspaces = append(cp.Workspaces, m.(*pb.WorkspaceRecord).Workspace)
	}); err != nil {
		return nil, err
	}
	if err := allRecords(ctx, q, "command", func() proto.Message { return &pb.CommandRecord{} }, func(m proto.Message) {
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
	return d.q.PruneEvents(context.Background(), workerdb.PruneEventsParams{
		Applied: applied, BeforeAt: before, Keep: int64(keep),
	})
}

type outboxItem struct {
	seq  int64
	kind string
	doc  []byte
}

// pending returns up to limit items of the old outbox not yet delivered.
func (d *db) pending(limit int) ([]outboxItem, error) {
	rows, err := d.q.ListPendingOutbox(context.Background(), int64(limit))
	if err != nil {
		return nil, err
	}
	out := make([]outboxItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, outboxItem{seq: r.Seq, kind: r.Kind, doc: []byte(r.Doc)})
	}
	return out, nil
}

// ack drops every outbox item up to seq.
func (d *db) ack(seq int64) error {
	return d.q.AckOutbox(context.Background(), seq)
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
	if err := d.q.UpdateMeta(context.Background(), workerdb.UpdateMetaParams{Value: epoch, Key: "epoch"}); err != nil {
		return err
	}
	d.epoch = epoch
	return nil
}

// eventAt is the time of the event numbered seq, or 0 when it is not kept.
func (d *db) eventAt(seq int64) (int64, error) {
	return d.q.GetEventAt(context.Background(), seq)
}
