package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// boardCmd is Hotbed's native project-scoped message board & task queue.
type boardCmd struct {
	Read     boardReadCmd     `cmd:"" help:"List threads in a project (no ID), or read one whole thread by message ID."`
	Post     boardPostCmd     `cmd:"" help:"Start a new thread in a project (or schedule one with --in/--at/--every)."`
	Reply    boardReplyCmd    `cmd:"" help:"Reply in an existing thread (or schedule a reply with --in/--at/--every)."`
	Edit     boardEditCmd     `cmd:"" help:"Replace a message's text (author or principal only)."`
	Delete   boardDeleteCmd   `cmd:"" aliases:"rm,del" help:"Soft-delete a message into a tombstone."`
	React    boardReactCmd    `cmd:"" help:"Add or remove (--remove) a normalized emoji reaction on a message."`
	Label    boardLabelCmd    `cmd:"" help:"Set or unset labels on a message, with optional atomic CAS preconditions (--if / --if-unset)."`
	Wait     boardWaitCmd     `cmd:"" help:"Wait for matching board events after a cursor (bounded long-poll slices)."`
	Tail     boardTailCmd     `cmd:"" help:"Stream matching board events live as they arrive."`
	Upload   boardUploadCmd   `cmd:"" help:"Upload a file attachment (up to 4 MiB) to reference in board posts or schedules."`
	Download boardDownloadCmd `cmd:"" help:"Download a board attachment by ID."`
	Actors   boardActorsCmd   `cmd:"" help:"List actors seen on the board."`
	Schema   boardSchemaCmd   `cmd:"" aliases:"schemas" help:"List, inspect, or register structured-data block schemas (json#name@version)."`
	Schedule boardScheduleCmd `cmd:"" aliases:"schedules" help:"Schedule, list, or cancel future/recurring board posts."`
	Task     boardTaskCmd     `cmd:"" aliases:"tasks" help:"First-class DAG task queue with fenced claim leases (ls, create, claim, run, renew, release, complete, fail, block, unblock, reopen, assign, needs, cancel)."`
	Import   boardImportCmd   `cmd:"" help:"Idempotently import channels, actors, schemas, messages, tasks, and attachments from an existing @vercel/board server."`
}

type boardActorFlags struct {
	sessionFlag
	As     string `name:"as" env:"HOTBED_BOARD_PROFILE" help:"Act as this profile name (e.g. reviewer, worker-1)."`
	AsUser bool   `name:"as-user" help:"Act directly as the authenticated human user instead of the bound agent session."`
	Model  string `env:"HOTBED_MODEL" help:"Model name to stamp on agent posts (default: session model)."`
}

func (f boardActorFlags) resolveSession(ctx context.Context, d deps) (string, error) {
	if f.AsUser {
		return "", nil
	}
	return f.sessionFlag.optional(ctx, d)
}

func parseBoardLabels(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, raw := range pairs {
		k, v, ok := strings.Cut(raw, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, api.Errorf(api.CodeInvalidArgs, "invalid label %q: expected key=value", raw)
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, nil
}

func parseBoardLabelFilters(items []string) ([]*pb.BoardLabelFilter, error) {
	if len(items) == 0 {
		return nil, nil
	}
	out := make([]*pb.BoardLabelFilter, 0, len(items))
	for _, raw := range items {
		k, v, hasEq := strings.Cut(raw, "=")
		k = strings.TrimSpace(k)
		if k == "" {
			return nil, api.Errorf(api.CodeInvalidArgs, "invalid label filter %q", raw)
		}
		f := &pb.BoardLabelFilter{Key: k}
		if hasEq {
			for _, part := range strings.Split(v, ",") {
				if p := strings.TrimSpace(part); p != "" {
					f.Values = append(f.Values, p)
				}
			}
		}
		out = append(out, f)
	}
	return out, nil
}

func readBoardText(d deps, args []string, filePath string) (string, error) {
	if filePath != "" {
		if len(args) > 0 {
			return "", api.Errorf(api.CodeInvalidArgs, "pass text either as arguments or with --file, not both")
		}
		var b []byte
		var err error
		if filePath == "-" {
			b, err = io.ReadAll(d.stdin)
		} else {
			b, err = os.ReadFile(filePath)
		}
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	if len(args) == 1 && args[0] == "-" {
		b, err := io.ReadAll(d.stdin)
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	return strings.Join(args, " "), nil
}

// parseBoardDuration parses human durations like "30s", "15m", "2h", "3d", "1w" into whole seconds.
func parseBoardDuration(raw string) (int64, error) {
	s := strings.TrimSpace(strings.ToLower(raw))
	if s == "" {
		return 0, nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n < 0 {
			return 0, api.Errorf(api.CodeInvalidArgs, "duration %q must not be negative", raw)
		}
		return n, nil
	}
	if strings.HasSuffix(s, "d") {
		n, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if err == nil && n >= 0 {
			return int64(n * 86400), nil
		}
	}
	if strings.HasSuffix(s, "w") {
		n, err := strconv.ParseFloat(strings.TrimSuffix(s, "w"), 64)
		if err == nil && n >= 0 {
			return int64(n * 7 * 86400), nil
		}
	}
	dur, err := time.ParseDuration(s)
	if err != nil || dur < 0 {
		return 0, api.Errorf(api.CodeInvalidArgs, "invalid duration %q (use e.g. 30s, 15m, 2h, 1d)", raw)
	}
	return int64(dur.Seconds()), nil
}

func parseBoardTimestampMs(raw string) (int64, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n < 1_000_000_000_000 {
			return n * 1000, nil
		}
		return n, nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UnixMilli(), nil
		}
	}
	return 0, api.Errorf(api.CodeInvalidArgs, "invalid timestamp %q (use RFC3339 like 2026-05-01T15:00:00Z)", raw)
}

func parseBoardTaskState(raw string) (pb.BoardTaskState, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "waiting":
		return pb.BoardTaskState_BOARD_TASK_STATE_WAITING, nil
	case "ready":
		return pb.BoardTaskState_BOARD_TASK_STATE_READY, nil
	case "claimed":
		return pb.BoardTaskState_BOARD_TASK_STATE_CLAIMED, nil
	case "blocked":
		return pb.BoardTaskState_BOARD_TASK_STATE_BLOCKED, nil
	case "completed", "done":
		return pb.BoardTaskState_BOARD_TASK_STATE_COMPLETED, nil
	case "failed":
		return pb.BoardTaskState_BOARD_TASK_STATE_FAILED, nil
	case "canceled", "cancelled":
		return pb.BoardTaskState_BOARD_TASK_STATE_CANCELED, nil
	default:
		return pb.BoardTaskState_BOARD_TASK_STATE_UNSPECIFIED, api.Errorf(api.CodeInvalidArgs, "unknown task state %q (expected waiting, ready, claimed, blocked, completed, failed, or canceled)", raw)
	}
}

func boardTaskStateLabel(st pb.BoardTaskState) string {
	switch st {
	case pb.BoardTaskState_BOARD_TASK_STATE_WAITING:
		return "waiting"
	case pb.BoardTaskState_BOARD_TASK_STATE_READY:
		return "ready"
	case pb.BoardTaskState_BOARD_TASK_STATE_CLAIMED:
		return "claimed"
	case pb.BoardTaskState_BOARD_TASK_STATE_BLOCKED:
		return "blocked"
	case pb.BoardTaskState_BOARD_TASK_STATE_COMPLETED:
		return "completed"
	case pb.BoardTaskState_BOARD_TASK_STATE_FAILED:
		return "failed"
	case pb.BoardTaskState_BOARD_TASK_STATE_CANCELED:
		return "canceled"
	default:
		return "-"
	}
}

// ---- Read ----

type boardReadCmd struct {
	controllerClient
	boardActorFlags
	projectFlag
	ID          int64    `arg:"" optional:"" help:"Message ID to read its whole thread (omit to list threads in the project)."`
	AllProjects bool     `name:"all-projects" help:"List threads across all projects."`
	Label       []string `short:"l" name:"label" help:"Filter by label (key, key=val, or key=v1,v2). Repeatable."`
	By          string   `help:"Filter by author handle or actor ID."`
	Mentions    string   `help:"Filter by @mention ('me', handle, or actor ID)."`
	Text        string   `help:"Case-insensitive text search."`
	After       int64    `help:"Only threads/replies with activity after this cursor or message ID."`
	Before      string   `help:"Opaque pagination cursor ('older') from a previous list."`
	Limit       int32    `help:"Maximum threads or replies to return."`
}

func (c *boardReadCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	labels, err := parseBoardLabelFilters(c.Label)
	if err != nil {
		return nil, err
	}
	if c.ID > 0 {
		return cl.board.GetBoardThread(ctx, &pb.GetBoardThreadRequest{
			Id:      c.ID,
			AfterId: c.After,
			Labels:  labels,
			Limit:   c.Limit,
		})
	}
	sc, err := c.listing(ctx, d, c.sessionFlag, c.AllProjects)
	if err != nil {
		return nil, err
	}
	sess := sc.session
	if c.AsUser {
		sess = ""
	}
	req := &pb.ListBoardThreadsRequest{
		SessionId: sess,
		Remote:    sc.remote,
		Labels:    labels,
		By:        c.By,
		Mentions:  c.Mentions,
		Text:      c.Text,
		After:     c.After,
		Before:    c.Before,
		Limit:     c.Limit,
		AsProfile: c.As,
	}
	switch {
	case sc.all:
		req.Scope = &pb.ListBoardThreadsRequest_AllProjects{AllProjects: true}
	case sc.project != "":
		req.Scope = &pb.ListBoardThreadsRequest_Project{Project: sc.project}
	}
	return cl.board.ListBoardThreads(ctx, req)
}

// ---- Post & Reply ----

type boardScheduleOpts struct {
	In    string `help:"Delay before sending (e.g. 5m, 2h, 1d)."`
	At    string `help:"Send at this RFC3339 timestamp."`
	Every string `help:"Repeat interval (e.g. 30m, 1d)."`
	Until string `help:"Stop repeating after this RFC3339 timestamp."`
	For   string `help:"Stop repeating after this duration from start (e.g. 24h)."`
	Times int32  `help:"Stop repeating after N sends."`
}

func (s boardScheduleOpts) active() bool {
	return s.In != "" || s.At != "" || s.Every != "" || s.Until != "" || s.For != "" || s.Times > 0
}

type boardPostCmd struct {
	controllerClient
	boardActorFlags
	projectFlag
	Text           []string `arg:"" optional:"" help:"Message text (or '-' to read stdin, or --file)."`
	File           string   `short:"f" help:"Read message text from file ('-' for stdin)."`
	Label          []string `short:"l" name:"label" help:"Set label key=value. Repeatable."`
	Attach         []string `name:"attach" help:"Attachment ID from 'hotbed board upload'. Repeatable."`
	IdempotencyKey string   `name:"idempotency-key" help:"Deduplication key scoped to your principal."`
	boardScheduleOpts
}

func (c *boardPostCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	text, err := readBoardText(d, c.Text, c.File)
	if err != nil {
		return nil, err
	}
	labels, err := parseBoardLabels(c.Label)
	if err != nil {
		return nil, err
	}
	sess, err := c.resolveSession(ctx, d)
	if err != nil {
		return nil, err
	}
	remote, _ := d.checkoutRemote(ctx)
	if c.boardScheduleOpts.active() {
		return runBoardScheduleCreate(ctx, cl, c.Project, 0, text, labels, c.Attach, sess, remote, c.As, c.Model, d.t3codeThreadID(), c.boardScheduleOpts)
	}
	return cl.board.PostBoardMessage(ctx, &pb.PostBoardMessageRequest{
		Project:        c.Project,
		Text:           text,
		Labels:         labels,
		AttachmentIds:  c.Attach,
		IdempotencyKey: c.IdempotencyKey,
		SessionId:      sess,
		Remote:         remote,
		AsProfile:      c.As,
		Model:          c.Model,
		T3CodeThreadId: d.t3codeThreadID(),
	})
}

type boardReplyCmd struct {
	controllerClient
	boardActorFlags
	ID             int64    `arg:"" help:"Thread or message ID to reply to."`
	Text           []string `arg:"" optional:"" help:"Reply text (or '-' to read stdin, or --file)."`
	File           string   `short:"f" help:"Read reply text from file ('-' for stdin)."`
	Label          []string `short:"l" name:"label" help:"Set label key=value. Repeatable."`
	Attach         []string `name:"attach" help:"Attachment ID from 'hotbed board upload'. Repeatable."`
	After          int64    `help:"Precondition: refuse with CONFLICT if a newer message exists in the thread after this ID."`
	IdempotencyKey string   `name:"idempotency-key" help:"Deduplication key scoped to your principal."`
	boardScheduleOpts
}

func (c *boardReplyCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	text, err := readBoardText(d, c.Text, c.File)
	if err != nil {
		return nil, err
	}
	labels, err := parseBoardLabels(c.Label)
	if err != nil {
		return nil, err
	}
	sess, err := c.resolveSession(ctx, d)
	if err != nil {
		return nil, err
	}
	if c.boardScheduleOpts.active() {
		return runBoardScheduleCreate(ctx, cl, "", c.ID, text, labels, c.Attach, sess, "", c.As, c.Model, d.t3codeThreadID(), c.boardScheduleOpts)
	}
	return cl.board.PostBoardMessage(ctx, &pb.PostBoardMessageRequest{
		Re:             c.ID,
		Text:           text,
		Labels:         labels,
		AttachmentIds:  c.Attach,
		AfterId:        c.After,
		IdempotencyKey: c.IdempotencyKey,
		SessionId:      sess,
		AsProfile:      c.As,
		Model:          c.Model,
		T3CodeThreadId: d.t3codeThreadID(),
	})
}

func runBoardScheduleCreate(
	ctx context.Context,
	cl clients,
	project string,
	re int64,
	text string,
	labels map[string]string,
	attach []string,
	sess, remote, asProfile, model, t3codeThreadID string,
	s boardScheduleOpts,
) (*pb.BoardScheduleResult, error) {
	inSec, err := parseBoardDuration(s.In)
	if err != nil {
		return nil, err
	}
	atMs, err := parseBoardTimestampMs(s.At)
	if err != nil {
		return nil, err
	}
	everySec, err := parseBoardDuration(s.Every)
	if err != nil {
		return nil, err
	}
	untilMs, err := parseBoardTimestampMs(s.Until)
	if err != nil {
		return nil, err
	}
	forSec, err := parseBoardDuration(s.For)
	if err != nil {
		return nil, err
	}
	return cl.board.ScheduleBoardMessage(ctx, &pb.ScheduleBoardMessageRequest{
		Project:        project,
		Re:             re,
		Text:           text,
		Labels:         labels,
		AttachmentIds:  attach,
		InSeconds:      inSec,
		AtMs:           atMs,
		EverySeconds:   everySec,
		UntilMs:        untilMs,
		ForSeconds:     forSec,
		Times:          s.Times,
		SessionId:      sess,
		Remote:         remote,
		AsProfile:      asProfile,
		Model:          model,
		T3CodeThreadId: t3codeThreadID,
	})
}

// ---- Edit, Delete, React, Label ----

type boardEditCmd struct {
	controllerClient
	boardActorFlags
	ID         int64    `arg:"" help:"Message ID to edit."`
	Text       []string `arg:"" optional:"" help:"New message text (or '-' for stdin, or --file)."`
	File       string   `short:"f" help:"Read new text from file ('-' for stdin)."`
	IfRevision int64    `name:"if-revision" help:"Refuse with CONFLICT unless the message is at this revision."`
}

func (c *boardEditCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	text, err := readBoardText(d, c.Text, c.File)
	if err != nil {
		return nil, err
	}
	sess, err := c.resolveSession(ctx, d)
	if err != nil {
		return nil, err
	}
	return cl.board.EditBoardMessage(ctx, &pb.EditBoardMessageRequest{
		Id:         c.ID,
		Text:       text,
		IfRevision: c.IfRevision,
		SessionId:  sess,
		AsProfile:  c.As,
	})
}

type boardDeleteCmd struct {
	controllerClient
	boardActorFlags
	ID         int64 `arg:"" help:"Message ID to delete."`
	IfRevision int64 `name:"if-revision" help:"Refuse with CONFLICT unless the message is at this revision."`
}

func (c *boardDeleteCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	sess, err := c.resolveSession(ctx, d)
	if err != nil {
		return nil, err
	}
	return cl.board.DeleteBoardMessage(ctx, &pb.DeleteBoardMessageRequest{
		Id:         c.ID,
		IfRevision: c.IfRevision,
		SessionId:  sess,
		AsProfile:  c.As,
	})
}

type boardReactCmd struct {
	controllerClient
	boardActorFlags
	ID     int64  `arg:"" help:"Message ID to react to."`
	Emoji  string `arg:"" help:"Emoji character or shortname (e.g. 👍, thumbsup, +1, eyes)."`
	Remove bool   `help:"Remove your reaction instead of adding it."`
}

func (c *boardReactCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	sess, err := c.resolveSession(ctx, d)
	if err != nil {
		return nil, err
	}
	return cl.board.ReactBoardMessage(ctx, &pb.ReactBoardMessageRequest{
		Id:        c.ID,
		Emoji:     c.Emoji,
		Remove:    c.Remove,
		SessionId: sess,
		AsProfile: c.As,
	})
}

type boardLabelCmd struct {
	controllerClient
	boardActorFlags
	ID      int64    `arg:"" help:"Message ID to label."`
	Changes []string `arg:"" optional:"" help:"Label changes: key=value to set, or -key to unset."`
	Set     []string `help:"Set label key=value. Repeatable."`
	Unset   []string `help:"Unset label key. Repeatable."`
	If      []string `name:"if" help:"CAS precondition: key=value must currently match. Repeatable."`
	IfUnset []string `name:"if-unset" help:"CAS precondition: key must currently be unset. Repeatable."`
}

func (c *boardLabelCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	setPairs := append([]string(nil), c.Set...)
	unsetKeys := append([]string(nil), c.Unset...)
	for _, raw := range c.Changes {
		if strings.HasPrefix(raw, "-") && !strings.Contains(raw, "=") {
			unsetKeys = append(unsetKeys, strings.TrimPrefix(raw, "-"))
		} else {
			setPairs = append(setPairs, raw)
		}
	}
	setMap, err := parseBoardLabels(setPairs)
	if err != nil {
		return nil, err
	}
	ifMap, err := parseBoardLabels(c.If)
	if err != nil {
		return nil, err
	}
	sess, err := c.resolveSession(ctx, d)
	if err != nil {
		return nil, err
	}
	return cl.board.LabelBoardMessage(ctx, &pb.LabelBoardMessageRequest{
		Id:        c.ID,
		Set:       setMap,
		Unset:     unsetKeys,
		IfEquals:  ifMap,
		IfUnset:   c.IfUnset,
		SessionId: sess,
		AsProfile: c.As,
	})
}

// ---- Wait & Tail ----

type boardEventFilterFlags struct {
	AllProjects bool     `name:"all-projects" help:"Watch events across all projects."`
	Re          int64    `help:"Only events in this thread ID."`
	Label       []string `short:"l" name:"label" help:"Only events matching this label filter. Repeatable."`
	By          string   `help:"Only events by this author handle or actor ID."`
	Mentions    string   `help:"Only events mentioning 'me', handle, or actor ID."`
	Text        string   `help:"Only events whose message contains this text."`
	All         bool     `name:"all" help:"Include bookkeeping events (task.renewed, actor.seen)."`
}

type boardWaitCmd struct {
	controllerClient
	boardActorFlags
	projectFlag
	boardEventFilterFlags
	After   int64  `help:"Start reading events strictly after this cursor."`
	Last    int32  `help:"Return the newest N matching events when --after is 0."`
	Timeout string `default:"30s" help:"How long to wait for a matching event (e.g. 30s, 5m)."`
	Limit   int32  `default:"50" help:"Maximum events to return."`
}

func (c *boardWaitCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.stream)
	if err != nil {
		return nil, err
	}
	sc, err := c.listing(ctx, d, c.sessionFlag, c.AllProjects)
	if err != nil {
		return nil, err
	}
	labels, err := parseBoardLabelFilters(c.Label)
	if err != nil {
		return nil, err
	}
	totalSec, err := parseBoardDuration(c.Timeout)
	if err != nil {
		return nil, err
	}
	sess := sc.session
	if c.AsUser {
		sess = ""
	}
	deadline := time.Now().Add(time.Duration(totalSec) * time.Second)
	cursor := c.After
	for {
		remaining := int32(time.Until(deadline).Seconds())
		if remaining < 0 {
			remaining = 0
		}
		sliceSec := remaining
		if sliceSec > 120 {
			sliceSec = 120
		}
		req := &pb.ListBoardEventsRequest{
			SessionId:      sess,
			Remote:         sc.remote,
			Re:             c.Re,
			Labels:         labels,
			By:             c.By,
			Mentions:       c.Mentions,
			Text:           c.Text,
			After:          cursor,
			Last:           c.Last,
			TimeoutSeconds: sliceSec,
			Limit:          c.Limit,
			IncludeAll:     c.All,
			AsProfile:      c.As,
		}
		switch {
		case sc.all:
			req.Scope = &pb.ListBoardEventsRequest_AllProjects{AllProjects: true}
		case sc.project != "":
			req.Scope = &pb.ListBoardEventsRequest_Project{Project: sc.project}
		}
		res, err := cl.board.ListBoardEvents(ctx, req)
		if err != nil {
			return nil, err
		}
		if len(res.Events) > 0 || time.Now().After(deadline) || totalSec == 0 || c.Last > 0 {
			return res, nil
		}
		if res.Cursor > cursor {
			cursor = res.Cursor
		}
	}
}

type boardTailCmd struct {
	controllerClient
	boardActorFlags
	projectFlag
	boardEventFilterFlags
	After int64 `help:"Start tailing after this cursor (default: current cursor)."`
	Last  int32 `help:"Print the last N matching events before tailing live."`
	Count int32 `help:"Stop after printing N events (0 = stream until interrupted)."`
}

func (c *boardTailCmd) Exec(context.Context, deps) (any, error) { return nil, nil }

func (c *boardTailCmd) Raw(ctx context.Context, d deps) (int, bool, error) {
	cl, err := c.clients(d.stream)
	if err != nil {
		return exitError, false, err
	}
	sc, err := c.listing(ctx, d, c.sessionFlag, c.AllProjects)
	if err != nil {
		return exitError, false, err
	}
	labels, err := parseBoardLabelFilters(c.Label)
	if err != nil {
		return exitError, false, err
	}
	sess := sc.session
	if c.AsUser {
		sess = ""
	}
	makeReq := func(after int64, last int32, waitSec int32) *pb.ListBoardEventsRequest {
		req := &pb.ListBoardEventsRequest{
			SessionId:      sess,
			Remote:         sc.remote,
			Re:             c.Re,
			Labels:         labels,
			By:             c.By,
			Mentions:       c.Mentions,
			Text:           c.Text,
			After:          after,
			Last:           last,
			TimeoutSeconds: waitSec,
			Limit:          100,
			IncludeAll:     c.All,
			AsProfile:      c.As,
		}
		switch {
		case sc.all:
			req.Scope = &pb.ListBoardEventsRequest_AllProjects{AllProjects: true}
		case sc.project != "":
			req.Scope = &pb.ListBoardEventsRequest_Project{Project: sc.project}
		}
		return req
	}

	printedAny := false
	emitted := int32(0)
	emit := func(ev *pb.BoardEvent) error {
		if ev.Message != nil {
			d.cockpit.fill(ev.Message)
		}
		if d.output.JSON {
			raw, err := protojson.MarshalOptions{UseProtoNames: false}.Marshal(ev)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(d.stdout, "%s\n", raw)
			return err
		}
		_, err := fmt.Fprintln(d.stdout, formatBoardEventLine(ev))
		return err
	}

	cursor := c.After
	if cursor == 0 {
		initLast := c.Last
		res, err := cl.board.ListBoardEvents(ctx, makeReq(0, initLast, 0))
		if err != nil {
			return exitError, printedAny, err
		}
		cursor = res.Cursor
		if initLast > 0 {
			for _, ev := range res.Events {
				if err := emit(ev); err != nil {
					return exitError, printedAny, err
				}
				printedAny = true
				emitted++
				if c.Count > 0 && emitted >= c.Count {
					return exitOK, printedAny, nil
				}
			}
		}
	}

	for {
		if ctx.Err() != nil {
			return exitOK, printedAny, nil
		}
		res, err := cl.board.ListBoardEvents(ctx, makeReq(cursor, 0, 60))
		if err != nil {
			if ctx.Err() != nil {
				return exitOK, printedAny, nil
			}
			return exitError, printedAny, err
		}
		if res.Cursor > cursor {
			cursor = res.Cursor
		}
		for _, ev := range res.Events {
			if err := emit(ev); err != nil {
				return exitError, printedAny, err
			}
			printedAny = true
			emitted++
			if c.Count > 0 && emitted >= c.Count {
				return exitOK, printedAny, nil
			}
		}
	}
}

// ---- Upload, Download, Actors ----

type boardUploadCmd struct {
	controllerClient
	boardActorFlags
	Path        string `arg:"" help:"Path to file to upload (or '-' to read stdin)."`
	Name        string `help:"Override stored filename (default: basename of path)."`
	ContentType string `name:"content-type" help:"Override MIME type (default: detected from filename/bytes)."`
}

func (c *boardUploadCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	var data []byte
	name := c.Name
	if c.Path == "-" {
		data, err = io.ReadAll(d.stdin)
		if name == "" {
			name = "stdin.bin"
		}
	} else {
		data, err = os.ReadFile(c.Path)
		if name == "" {
			name = filepath.Base(c.Path)
		}
	}
	if err != nil {
		return nil, err
	}
	ct := strings.TrimSpace(c.ContentType)
	if ct == "" {
		ct = mime.TypeByExtension(filepath.Ext(name))
	}
	if ct == "" {
		ct = http.DetectContentType(data)
	}
	sess, err := c.resolveSession(ctx, d)
	if err != nil {
		return nil, err
	}
	return cl.board.UploadBoardAttachment(ctx, &pb.UploadBoardAttachmentRequest{
		Name:        name,
		ContentType: ct,
		Data:        data,
		SessionId:   sess,
		AsProfile:   c.As,
	})
}

type boardDownloadCmd struct {
	controllerClient
	ID  string `arg:"" help:"Attachment ID to download."`
	Out string `short:"o" help:"Write bytes to this file path (default: print to stdout, or JSON metadata with --json)."`
}

func (c *boardDownloadCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	res, err := cl.board.DownloadBoardAttachment(ctx, &pb.DownloadBoardAttachmentRequest{Id: c.ID})
	if err != nil {
		return nil, err
	}
	if c.Out != "" {
		if err := os.WriteFile(c.Out, res.Data, 0o644); err != nil {
			return nil, err
		}
		return map[string]any{
			"id":          res.Attachment.GetId(),
			"name":        res.Attachment.GetName(),
			"contentType": res.Attachment.GetContentType(),
			"size":        len(res.Data),
			"path":        c.Out,
		}, nil
	}
	if d.output.machine() {
		return res, nil
	}
	_, err = d.stdout.Write(res.Data)
	return nil, err
}

type boardActorsCmd struct {
	controllerClient
	boardActorFlags
	projectFlag
	AllProjects bool     `name:"all-projects" help:"List actors across all projects."`
	ID          []string `name:"id" help:"Filter to specific actor ID(s). Repeatable."`
}

func (c *boardActorsCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	sc, err := c.listing(ctx, d, c.sessionFlag, c.AllProjects)
	if err != nil {
		return nil, err
	}
	req := &pb.ListBoardActorsRequest{
		SessionId: sc.session,
		Remote:    sc.remote,
		Ids:       c.ID,
	}
	switch {
	case sc.all:
		req.Scope = &pb.ListBoardActorsRequest_AllProjects{AllProjects: true}
	case sc.project != "":
		req.Scope = &pb.ListBoardActorsRequest_Project{Project: sc.project}
	}
	return cl.board.ListBoardActors(ctx, req)
}

// ---- Schemas ----

type boardSchemaCmd struct {
	Ls     boardSchemaLsCmd     `cmd:"" help:"List built-in and custom structured-data schemas."`
	Show   boardSchemaShowCmd   `cmd:"" help:"Show one schema definition by name@version."`
	Create boardSchemaCreateCmd `cmd:"" help:"Register a new immutable schema version (name@version)."`
}

type boardSchemaLsCmd struct {
	controllerClient
}

func (c *boardSchemaLsCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.board.ListBoardSchemas(ctx, &pb.ListBoardSchemasRequest{})
}

type boardSchemaShowCmd struct {
	controllerClient
	ID string `arg:"" help:"Schema ID (name@version, e.g. git-commit@1)."`
}

func (c *boardSchemaShowCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.board.GetBoardSchema(ctx, &pb.GetBoardSchemaRequest{Id: c.ID})
}

type boardSchemaCreateCmd struct {
	controllerClient
	boardActorFlags
	ID          string `arg:"" help:"Schema name@version (e.g. bench-result@1)."`
	Description string `short:"d" help:"Human-readable description of this schema."`
	Schema      string `help:"Inline JSON Schema string."`
	File        string `short:"f" help:"Read JSON Schema from file ('-' for stdin)."`
}

func (c *boardSchemaCreateCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	name, verStr, ok := strings.Cut(strings.TrimSpace(c.ID), "@")
	if !ok || name == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "schema ID %q must be name@version (e.g. deploy@1)", c.ID)
	}
	ver, err := strconv.Atoi(verStr)
	if err != nil || ver <= 0 {
		return nil, api.Errorf(api.CodeInvalidArgs, "invalid schema version %q", verStr)
	}
	var raw string
	if c.File != "" {
		var b []byte
		if c.File == "-" {
			b, err = io.ReadAll(d.stdin)
		} else {
			b, err = os.ReadFile(c.File)
		}
		if err != nil {
			return nil, err
		}
		raw = string(b)
	} else {
		raw = c.Schema
	}
	if strings.TrimSpace(raw) == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "schema JSON is required (--schema or --file)")
	}
	sess, err := c.resolveSession(ctx, d)
	if err != nil {
		return nil, err
	}
	return cl.board.CreateBoardSchema(ctx, &pb.CreateBoardSchemaRequest{
		Name:        name,
		Version:     int32(ver),
		Description: c.Description,
		SchemaJson:  raw,
		SessionId:   sess,
		AsProfile:   c.As,
	})
}

// ---- Schedules ----

type boardScheduleCmd struct {
	Add    boardScheduleAddCmd    `cmd:"" help:"Schedule a future or recurring board post or reply."`
	Ls     boardScheduleLsCmd     `cmd:"" help:"List pending (or --all) scheduled board messages."`
	Cancel boardScheduleCancelCmd `cmd:"" help:"Cancel a pending scheduled board message."`
}

type boardScheduleAddCmd struct {
	controllerClient
	boardActorFlags
	projectFlag
	Text   []string `arg:"" optional:"" help:"Message text (or '-' for stdin, or --file)."`
	File   string   `short:"f" help:"Read message text from file ('-' for stdin)."`
	Re     int64    `help:"Thread ID to reply to (omit to start a new thread)."`
	Label  []string `short:"l" name:"label" help:"Set label key=value. Repeatable."`
	Attach []string `name:"attach" help:"Attachment ID. Repeatable."`
	boardScheduleOpts
}

func (c *boardScheduleAddCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	text, err := readBoardText(d, c.Text, c.File)
	if err != nil {
		return nil, err
	}
	labels, err := parseBoardLabels(c.Label)
	if err != nil {
		return nil, err
	}
	sess, err := c.resolveSession(ctx, d)
	if err != nil {
		return nil, err
	}
	remote, _ := d.checkoutRemote(ctx)
	return runBoardScheduleCreate(ctx, cl, c.Project, c.Re, text, labels, c.Attach, sess, remote, c.As, c.Model, d.t3codeThreadID(), c.boardScheduleOpts)
}

type boardScheduleLsCmd struct {
	controllerClient
	boardActorFlags
	projectFlag
	AllProjects bool   `name:"all-projects" help:"List schedules across all projects."`
	Re          int64  `help:"Filter to schedules targeting this thread ID."`
	By          string `help:"Filter by author handle or actor ID."`
	All         bool   `name:"all" help:"Include completed and canceled schedules."`
}

func (c *boardScheduleLsCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	sc, err := c.listing(ctx, d, c.sessionFlag, c.AllProjects)
	if err != nil {
		return nil, err
	}
	req := &pb.ListBoardSchedulesRequest{
		SessionId:  sc.session,
		Remote:     sc.remote,
		Re:         c.Re,
		By:         c.By,
		IncludeAll: c.All,
	}
	switch {
	case sc.all:
		req.Scope = &pb.ListBoardSchedulesRequest_AllProjects{AllProjects: true}
	case sc.project != "":
		req.Scope = &pb.ListBoardSchedulesRequest_Project{Project: sc.project}
	}
	return cl.board.ListBoardSchedules(ctx, req)
}

type boardScheduleCancelCmd struct {
	controllerClient
	boardActorFlags
	ID int64 `arg:"" help:"Schedule ID to cancel."`
}

func (c *boardScheduleCancelCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	sess, err := c.resolveSession(ctx, d)
	if err != nil {
		return nil, err
	}
	return cl.board.CancelBoardSchedule(ctx, &pb.CancelBoardScheduleRequest{
		Id:        c.ID,
		SessionId: sess,
		AsProfile: c.As,
	})
}

// ---- Tasks ----

type boardTaskCmd struct {
	Ls       boardTaskLsCmd       `cmd:"" help:"List tasks (default: open states waiting/ready/claimed/blocked)."`
	Create   boardTaskCreateCmd   `cmd:"" help:"Create a task (or adopt an existing thread with --re, or batch-create a DAG plan with --plan)."`
	Claim    boardTaskClaimCmd    `cmd:"" help:"Claim a specific task by ID, or claim the oldest ready task (--ready)."`
	Run      boardTaskRunCmd      `cmd:"" help:"Claim a task, run a command with HOTBED_BOARD_TASK and automatic lease renewal, and release on signal."`
	Renew    boardTaskRenewCmd    `cmd:"" help:"Extend a held task claim's lease (defaults to preserving the original lease duration)."`
	Release  boardTaskReleaseCmd  `cmd:"" help:"Release a held task back to ready."`
	Complete boardTaskCompleteCmd `cmd:"" aliases:"done" help:"Complete a held task and ready dependent waiting tasks."`
	Fail     boardTaskFailCmd     `cmd:"" help:"Fail a held task with a reason."`
	Block    boardTaskBlockCmd    `cmd:"" help:"Hold a task for human intervention."`
	Unblock  boardTaskUnblockCmd  `cmd:"" help:"Lift a hold on a blocked task."`
	Reopen   boardTaskReopenCmd   `cmd:"" help:"Reopen a failed task back to ready."`
	Assign   boardTaskAssignCmd   `cmd:"" help:"Assign a task to an actor/profile (or --unassign back to the pool)."`
	Needs    boardTaskNeedsCmd    `cmd:"" help:"Replace a task's prerequisite task IDs (with cycle detection)."`
	Cancel   boardTaskCancelCmd   `cmd:"" help:"Cancel a non-completed task with a reason."`
}

type boardTaskLsCmd struct {
	controllerClient
	boardActorFlags
	projectFlag
	AllProjects bool     `name:"all-projects" help:"List tasks across all projects."`
	State       []string `help:"Filter by state (waiting, ready, claimed, blocked, completed, failed, canceled). Repeatable."`
	All         bool     `name:"all" help:"Include completed, failed, and canceled tasks."`
	Label       []string `short:"l" name:"label" help:"Filter by label. Repeatable."`
	By          string   `help:"Filter by author."`
	Owner       string   `help:"Filter by task owner."`
	For         string   `help:"Filter by assignee ('me', handle, or actor ID)."`
	Needs       int64    `help:"Only tasks that depend on this prerequisite task ID."`
	After       int64    `help:"Only tasks with activity after this cursor."`
	Limit       int32    `help:"Maximum tasks to return."`
	Latest      bool     `help:"Order by newest activity first instead of oldest first."`
}

func (c *boardTaskLsCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	sc, err := c.listing(ctx, d, c.sessionFlag, c.AllProjects)
	if err != nil {
		return nil, err
	}
	labels, err := parseBoardLabelFilters(c.Label)
	if err != nil {
		return nil, err
	}
	var states []pb.BoardTaskState
	for _, s := range c.State {
		st, err := parseBoardTaskState(s)
		if err != nil {
			return nil, err
		}
		states = append(states, st)
	}
	sess := sc.session
	if c.AsUser {
		sess = ""
	}
	req := &pb.ListBoardTasksRequest{
		SessionId:  sess,
		Remote:     sc.remote,
		States:     states,
		IncludeAll: c.All,
		Labels:     labels,
		By:         c.By,
		Owner:      c.Owner,
		ForActor:   c.For,
		Needs:      c.Needs,
		After:      c.After,
		Limit:      c.Limit,
		Latest:     c.Latest,
	}
	switch {
	case sc.all:
		req.Scope = &pb.ListBoardTasksRequest_AllProjects{AllProjects: true}
	case sc.project != "":
		req.Scope = &pb.ListBoardTasksRequest_Project{Project: sc.project}
	}
	return cl.board.ListBoardTasks(ctx, req)
}

type boardPlanTaskJSON struct {
	Key         string            `json:"key"`
	Text        string            `json:"text"`
	Labels      map[string]string `json:"labels"`
	Attachments []string          `json:"attachments"`
	For         string            `json:"for"`
	Needs       []any             `json:"needs"`
}

type boardTaskCreateCmd struct {
	controllerClient
	boardActorFlags
	projectFlag
	Text           []string `arg:"" optional:"" help:"Task text (or '-' for stdin, or --file)."`
	File           string   `short:"f" help:"Read task text from file ('-' for stdin)."`
	Plan           string   `help:"Path to JSON file ('-' for stdin) describing a batch plan of up to 100 tasks with internal key DAG needs."`
	Re             int64    `help:"Adopt an existing thread ID as a task instead of creating a new message."`
	Label          []string `short:"l" name:"label" help:"Set label key=value. Repeatable."`
	Attach         []string `name:"attach" help:"Attachment ID. Repeatable."`
	Needs          []int64  `help:"Prerequisite task ID(s). Repeatable."`
	For            string   `help:"Assign task to this handle/actor ID."`
	Claim          bool     `help:"Atomically claim the task upon creation."`
	Lease          string   `help:"Claim lease duration when --claim is set (e.g. 15m, 1h)."`
	IdempotencyKey string   `name:"idempotency-key" help:"Deduplication key."`
}

func (c *boardTaskCreateCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	sess, err := c.resolveSession(ctx, d)
	if err != nil {
		return nil, err
	}
	remote, _ := d.checkoutRemote(ctx)

	if c.Plan != "" {
		var raw []byte
		if c.Plan == "-" {
			raw, err = io.ReadAll(d.stdin)
		} else {
			raw, err = os.ReadFile(c.Plan)
		}
		if err != nil {
			return nil, err
		}
		var specs []boardPlanTaskJSON
		if err := json.Unmarshal(raw, &specs); err != nil {
			var wrapper struct {
				Tasks []boardPlanTaskJSON `json:"tasks"`
			}
			if werr := json.Unmarshal(raw, &wrapper); werr != nil || len(wrapper.Tasks) == 0 {
				return nil, api.Errorf(api.CodeInvalidArgs, "invalid --plan JSON: %v", err)
			}
			specs = wrapper.Tasks
		}
		pbSpecs := make([]*pb.BatchBoardTaskSpec, 0, len(specs))
		for _, s := range specs {
			spec := &pb.BatchBoardTaskSpec{
				Key:           s.Key,
				Text:          s.Text,
				Labels:        s.Labels,
				AttachmentIds: s.Attachments,
				ForActor:      s.For,
			}
			for _, n := range s.Needs {
				switch v := n.(type) {
				case float64:
					spec.Needs = append(spec.Needs, &pb.BatchBoardTaskNeed{Ref: &pb.BatchBoardTaskNeed_Id{Id: int64(v)}})
				case string:
					if idNum, perr := strconv.ParseInt(strings.TrimPrefix(v, "#"), 10, 64); perr == nil && idNum > 0 {
						spec.Needs = append(spec.Needs, &pb.BatchBoardTaskNeed{Ref: &pb.BatchBoardTaskNeed_Id{Id: idNum}})
					} else {
						spec.Needs = append(spec.Needs, &pb.BatchBoardTaskNeed{Ref: &pb.BatchBoardTaskNeed_Key{Key: v}})
					}
				default:
					return nil, api.Errorf(api.CodeInvalidArgs, "invalid need %v in plan task %q", n, s.Key)
				}
			}
			pbSpecs = append(pbSpecs, spec)
		}
		return cl.board.BatchCreateBoardTasks(ctx, &pb.BatchCreateBoardTasksRequest{
			Project:        c.Project,
			Tasks:          pbSpecs,
			IdempotencyKey: c.IdempotencyKey,
			SessionId:      sess,
			Remote:         remote,
			AsProfile:      c.As,
			Model:          c.Model,
			T3CodeThreadId: d.t3codeThreadID(),
		})
	}

	text, err := readBoardText(d, c.Text, c.File)
	if err != nil {
		return nil, err
	}
	labels, err := parseBoardLabels(c.Label)
	if err != nil {
		return nil, err
	}
	leaseSec, err := parseBoardDuration(c.Lease)
	if err != nil {
		return nil, err
	}
	res, err := cl.board.CreateBoardTask(ctx, &pb.CreateBoardTaskRequest{
		Project:        c.Project,
		Re:             c.Re,
		Text:           text,
		Labels:         labels,
		AttachmentIds:  c.Attach,
		IdempotencyKey: c.IdempotencyKey,
		Needs:          c.Needs,
		ForActor:       c.For,
		Claim:          c.Claim,
		LeaseSeconds:   int32(leaseSec),
		SessionId:      sess,
		Remote:         remote,
		AsProfile:      c.As,
		Model:          c.Model,
		T3CodeThreadId: d.t3codeThreadID(),
	})
	if err != nil {
		return nil, err
	}
	if res.Claim != nil && res.Claim.Token != "" && res.Task != nil {
		if err := saveBoardClaim(d, c.Controller, res.Task.Id, sess, c.As, boardClaimRecord{
			TaskID: res.Task.Id,
			Epoch:  res.Claim.Epoch,
			Token:  res.Claim.Token,
			Until:  res.Claim.Until,
		}); err != nil {
			return nil, err
		}
		res.Claim.Token = ""
	}
	return res, nil
}

type boardTaskClaimCmd struct {
	controllerClient
	boardActorFlags
	projectFlag
	ID          int64    `arg:"" optional:"" help:"Specific task ID to claim (omit with --ready to claim oldest ready task)."`
	Ready       bool     `help:"Claim the oldest ready task matching filters."`
	AllProjects bool     `name:"all-projects" help:"Claim from ready tasks across all projects."`
	Label       []string `short:"l" name:"label" help:"Filter ready tasks by label. Repeatable."`
	From        []string `help:"Filter ready tasks by owner(s). Repeatable."`
	Lease       string   `help:"Lease duration (default 15m, e.g. 5m, 1h)."`
	Wait        string   `help:"Wait up to this duration for a ready task (with --ready)."`
	PrintToken  bool     `name:"print-token" help:"Include the claim token in output instead of redacting it (it is always saved locally with mode 0600)."`
}

func (c *boardTaskClaimCmd) Exec(ctx context.Context, d deps) (any, error) {
	if c.ID == 0 && !c.Ready {
		return nil, api.Errorf(api.CodeInvalidArgs, "pass a task ID or --ready")
	}
	hc := d.http
	waitSec, err := parseBoardDuration(c.Wait)
	if err != nil {
		return nil, err
	}
	if waitSec > 0 {
		hc = d.stream
	}
	cl, err := c.clients(hc)
	if err != nil {
		return nil, err
	}
	leaseSec, err := parseBoardDuration(c.Lease)
	if err != nil {
		return nil, err
	}
	labels, err := parseBoardLabelFilters(c.Label)
	if err != nil {
		return nil, err
	}
	sess, err := c.resolveSession(ctx, d)
	if err != nil {
		return nil, err
	}
	req := &pb.ClaimBoardTaskRequest{
		Id:           c.ID,
		Ready:        c.Ready,
		SessionId:    sess,
		Labels:       labels,
		FromOwners:   c.From,
		LeaseSeconds: int32(leaseSec),
		WaitSeconds:  int32(waitSec),
		AsProfile:    c.As,
	}
	if c.Ready {
		sc, err := c.listing(ctx, d, c.sessionFlag, c.AllProjects)
		if err != nil {
			return nil, err
		}
		req.Remote = sc.remote
		switch {
		case sc.all:
			req.Scope = &pb.ClaimBoardTaskRequest_AllProjects{AllProjects: true}
		case sc.project != "":
			req.Scope = &pb.ClaimBoardTaskRequest_Project{Project: sc.project}
		}
	}
	res, err := cl.board.ClaimBoardTask(ctx, req)
	if err != nil {
		return nil, err
	}
	if res.Claim != nil && res.Claim.Token != "" && res.Task != nil {
		if err := saveBoardClaim(d, c.Controller, res.Task.Id, sess, c.As, boardClaimRecord{
			TaskID: res.Task.Id,
			Epoch:  res.Claim.Epoch,
			Token:  res.Claim.Token,
			Until:  res.Claim.Until,
		}); err != nil {
			return nil, err
		}
		if !c.PrintToken {
			res.Claim.Token = ""
		}
	}
	return res, nil
}

type boardTaskRunCmd struct {
	controllerClient
	boardActorFlags
	projectFlag
	ID          int64    `arg:"" optional:"" help:"Specific task ID to claim and run (omit with --ready to claim oldest ready task)."`
	Ready       bool     `help:"Claim the oldest ready task matching filters."`
	AllProjects bool     `name:"all-projects" help:"Claim from ready tasks across all projects."`
	Label       []string `short:"l" name:"label" help:"Filter ready tasks by label. Repeatable."`
	From        []string `help:"Filter ready tasks by owner(s). Repeatable."`
	Lease       string   `default:"15m" help:"Claim lease duration (renewed automatically every 1/3 lease)."`
	Wait        string   `help:"Wait up to this duration for a ready task (with --ready)."`
	dashArgs    []string `kong:"-"`
}

func (c *boardTaskRunCmd) SetDashArgs(args []string) []string {
	c.dashArgs = args
	return nil
}

func (c *boardTaskRunCmd) Exec(context.Context, deps) (any, error) { return nil, nil }

func (c *boardTaskRunCmd) Raw(ctx context.Context, d deps) (int, bool, error) {
	if len(c.dashArgs) == 0 {
		return exitInvalidArgs, false, api.Errorf(api.CodeInvalidArgs, "pass the command to run after --")
	}
	// The command runs where this does.
	wd, err := d.getwd()
	if err != nil {
		return exitError, false, err
	}
	claimCmd := &boardTaskClaimCmd{
		controllerClient: c.controllerClient,
		boardActorFlags:  c.boardActorFlags,
		projectFlag:      c.projectFlag,
		ID:               c.ID,
		Ready:            c.Ready,
		AllProjects:      c.AllProjects,
		Label:            c.Label,
		From:             c.From,
		Lease:            c.Lease,
		Wait:             c.Wait,
		PrintToken:       true,
	}
	rawRes, err := claimCmd.Exec(ctx, d)
	if err != nil {
		return exitError, false, err
	}
	res := rawRes.(*pb.BoardTaskResult)
	if res.Task == nil || res.Claim == nil {
		return exitError, false, api.Errorf(api.CodeInternal, "claim returned no task or claim")
	}
	sess, _ := c.resolveSession(ctx, d)
	taskID := res.Task.Id
	epoch := res.Claim.Epoch
	token := res.Claim.Token

	leaseSec, _ := parseBoardDuration(c.Lease)
	if leaseSec <= 0 {
		leaseSec = 900
	}
	renewEvery := time.Duration(max(leaseSec/3, 10)) * time.Second

	envPayload, _ := json.Marshal(map[string]any{
		"id":      taskID,
		"project": res.Task.Project,
		"text":    res.Task.Text,
		"epoch":   epoch,
		"token":   token,
		"until":   res.Claim.Until,
	})

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Background lease renewal loop
	cl, err := c.clients(d.http)
	if err != nil {
		return exitError, false, err
	}
	go func() {
		t := time.NewTicker(renewEvery)
		defer t.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
				_, rerr := cl.board.RenewBoardTask(runCtx, &pb.RenewBoardTaskRequest{
					Id:        taskID,
					Epoch:     epoch,
					Token:     token,
					SessionId: sess,
					AsProfile: c.As,
				})
				if rerr != nil {
					return
				}
			}
		}
	}()

	cmd := exec.CommandContext(runCtx, c.dashArgs[0], c.dashArgs[1:]...)
	cmd.Dir = wd
	cmd.Stdin = d.stdin
	cmd.Stdout = d.stdout
	cmd.Stderr = d.stderr
	cmd.Env = append(os.Environ(), "HOTBED_BOARD_TASK="+string(envPayload))

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	if err := cmd.Start(); err != nil {
		cancel()
		_, _ = cl.board.ReleaseBoardTask(context.Background(), &pb.ReleaseBoardTaskRequest{
			Id: taskID, Epoch: epoch, Token: token, SessionId: sess, AsProfile: c.As,
		})
		deleteBoardClaim(d, c.Controller, taskID, sess, c.As)
		return exitError, false, err
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	select {
	case err := <-waitDone:
		cancel()
		if err == nil {
			return exitOK, true, nil
		}
		var ee *exec.ExitError
		if ok := slices.Contains([]bool{err != nil}, true); ok {
			if e, ok := err.(*exec.ExitError); ok {
				ee = e
			}
		}
		if ee != nil {
			return ee.ExitCode(), true, nil
		}
		return exitError, true, err
	case <-sigCh:
		cancel()
		_ = cmd.Process.Signal(syscall.SIGTERM)
		<-waitDone
		_, _ = cl.board.ReleaseBoardTask(context.Background(), &pb.ReleaseBoardTaskRequest{
			Id: taskID, Epoch: epoch, Token: token, SessionId: sess, AsProfile: c.As,
		})
		deleteBoardClaim(d, c.Controller, taskID, sess, c.As)
		return exitInterrupted, true, nil
	}
}

type boardTaskFenceFlags struct {
	Epoch int32  `help:"Claim epoch (default: from local claim store or HOTBED_BOARD_TASK)."`
	Token string `help:"Claim token (default: from local claim store or HOTBED_BOARD_TASK)."`
}

type boardTaskRenewCmd struct {
	controllerClient
	boardActorFlags
	boardTaskFenceFlags
	ID    int64  `arg:"" help:"Task ID to renew."`
	Lease string `help:"Optional new lease duration (omit to preserve the claim's original lease duration)."`
}

func (c *boardTaskRenewCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	sess, err := c.resolveSession(ctx, d)
	if err != nil {
		return nil, err
	}
	epoch, token, err := resolveBoardClaimFence(d, c.Controller, c.ID, sess, c.As, c.Epoch, c.Token)
	if err != nil {
		return nil, err
	}
	leaseSec, err := parseBoardDuration(c.Lease)
	if err != nil {
		return nil, err
	}
	res, err := cl.board.RenewBoardTask(ctx, &pb.RenewBoardTaskRequest{
		Id:           c.ID,
		Epoch:        epoch,
		Token:        token,
		LeaseSeconds: int32(leaseSec),
		SessionId:    sess,
		AsProfile:    c.As,
	})
	if err != nil {
		return nil, err
	}
	if res.Claim != nil {
		_ = saveBoardClaim(d, c.Controller, c.ID, sess, c.As, boardClaimRecord{
			TaskID: c.ID,
			Epoch:  epoch,
			Token:  token,
			Until:  res.Claim.Until,
		})
		res.Claim.Token = ""
	}
	return res, nil
}

type boardTaskReleaseCmd struct {
	controllerClient
	boardActorFlags
	boardTaskFenceFlags
	ID int64 `arg:"" help:"Task ID to release back to ready."`
}

func (c *boardTaskReleaseCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	sess, err := c.resolveSession(ctx, d)
	if err != nil {
		return nil, err
	}
	epoch, token, err := resolveBoardClaimFence(d, c.Controller, c.ID, sess, c.As, c.Epoch, c.Token)
	if err != nil {
		return nil, err
	}
	res, err := cl.board.ReleaseBoardTask(ctx, &pb.ReleaseBoardTaskRequest{
		Id:        c.ID,
		Epoch:     epoch,
		Token:     token,
		SessionId: sess,
		AsProfile: c.As,
	})
	if err == nil {
		deleteBoardClaim(d, c.Controller, c.ID, sess, c.As)
	}
	return res, err
}

type boardTaskCompleteCmd struct {
	controllerClient
	boardActorFlags
	boardTaskFenceFlags
	ID     int64    `arg:"" help:"Task ID to complete."`
	Result []string `arg:"" optional:"" help:"Optional completion summary."`
	Note   string   `help:"Optional completion summary (alias for positional result)."`
}

func (c *boardTaskCompleteCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	sess, err := c.resolveSession(ctx, d)
	if err != nil {
		return nil, err
	}
	epoch, token, err := resolveBoardClaimFence(d, c.Controller, c.ID, sess, c.As, c.Epoch, c.Token)
	if err != nil {
		return nil, err
	}
	result := strings.TrimSpace(c.Note)
	if result == "" {
		result = strings.TrimSpace(strings.Join(c.Result, " "))
	}
	res, err := cl.board.CompleteBoardTask(ctx, &pb.CompleteBoardTaskRequest{
		Id:        c.ID,
		Epoch:     epoch,
		Token:     token,
		Result:    result,
		SessionId: sess,
		AsProfile: c.As,
	})
	if err == nil {
		deleteBoardClaim(d, c.Controller, c.ID, sess, c.As)
	}
	return res, err
}

type boardTaskFailCmd struct {
	controllerClient
	boardActorFlags
	boardTaskFenceFlags
	ID     int64    `arg:"" help:"Task ID to fail."`
	Reason []string `arg:"" optional:"" help:"Why the task failed."`
}

func (c *boardTaskFailCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	sess, err := c.resolveSession(ctx, d)
	if err != nil {
		return nil, err
	}
	epoch, token, err := resolveBoardClaimFence(d, c.Controller, c.ID, sess, c.As, c.Epoch, c.Token)
	if err != nil {
		return nil, err
	}
	res, err := cl.board.FailBoardTask(ctx, &pb.FailBoardTaskRequest{
		Id:        c.ID,
		Epoch:     epoch,
		Token:     token,
		Reason:    strings.TrimSpace(strings.Join(c.Reason, " ")),
		SessionId: sess,
		AsProfile: c.As,
	})
	if err == nil {
		deleteBoardClaim(d, c.Controller, c.ID, sess, c.As)
	}
	return res, err
}

type boardTaskBlockCmd struct {
	controllerClient
	boardActorFlags
	boardTaskFenceFlags
	ID     int64    `arg:"" help:"Task ID to hold."`
	Reason []string `arg:"" optional:"" help:"Why the task is blocked."`
	Kind   string   `help:"Optional hold category (e.g. needs-review, waiting-vendor)."`
}

func (c *boardTaskBlockCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	sess, err := c.resolveSession(ctx, d)
	if err != nil {
		return nil, err
	}
	epoch, token := c.Epoch, c.Token
	if token == "" {
		if e, t, ferr := resolveBoardClaimFence(d, c.Controller, c.ID, sess, c.As, epoch, ""); ferr == nil {
			epoch, token = e, t
		}
	}
	res, err := cl.board.BlockBoardTask(ctx, &pb.BlockBoardTaskRequest{
		Id:        c.ID,
		Reason:    strings.TrimSpace(strings.Join(c.Reason, " ")),
		Kind:      c.Kind,
		Epoch:     epoch,
		Token:     token,
		SessionId: sess,
		AsProfile: c.As,
	})
	if err == nil && token != "" {
		deleteBoardClaim(d, c.Controller, c.ID, sess, c.As)
	}
	return res, err
}

type boardTaskUnblockCmd struct {
	controllerClient
	boardActorFlags
	ID int64 `arg:"" help:"Blocked task ID to unblock."`
}

func (c *boardTaskUnblockCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	sess, err := c.resolveSession(ctx, d)
	if err != nil {
		return nil, err
	}
	return cl.board.UnblockBoardTask(ctx, &pb.UnblockBoardTaskRequest{
		Id:        c.ID,
		SessionId: sess,
		AsProfile: c.As,
	})
}

type boardTaskReopenCmd struct {
	controllerClient
	boardActorFlags
	ID int64 `arg:"" help:"Failed task ID to reopen back to ready."`
}

func (c *boardTaskReopenCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	sess, err := c.resolveSession(ctx, d)
	if err != nil {
		return nil, err
	}
	return cl.board.ReopenBoardTask(ctx, &pb.ReopenBoardTaskRequest{
		Id:        c.ID,
		SessionId: sess,
		AsProfile: c.As,
	})
}

type boardTaskAssignCmd struct {
	controllerClient
	boardActorFlags
	ID       int64  `arg:"" help:"Task ID to assign or unassign."`
	For      string `arg:"" optional:"" help:"Handle or actor ID to assign to (omit with --unassign)."`
	Unassign bool   `help:"Clear assignment and return task to the pool."`
}

func (c *boardTaskAssignCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	sess, err := c.resolveSession(ctx, d)
	if err != nil {
		return nil, err
	}
	return cl.board.AssignBoardTask(ctx, &pb.AssignBoardTaskRequest{
		Id:        c.ID,
		ForActor:  c.For,
		Unassign:  c.Unassign,
		SessionId: sess,
		AsProfile: c.As,
	})
}

type boardTaskNeedsCmd struct {
	controllerClient
	boardActorFlags
	ID    int64   `arg:"" help:"Task ID whose prerequisites to replace."`
	Needs []int64 `arg:"" optional:"" help:"New list of prerequisite task IDs (omit to clear all needs)."`
}

func (c *boardTaskNeedsCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	sess, err := c.resolveSession(ctx, d)
	if err != nil {
		return nil, err
	}
	return cl.board.SetBoardTaskNeeds(ctx, &pb.SetBoardTaskNeedsRequest{
		Id:        c.ID,
		Needs:     c.Needs,
		SessionId: sess,
		AsProfile: c.As,
	})
}

type boardTaskCancelCmd struct {
	controllerClient
	boardActorFlags
	ID     int64    `arg:"" help:"Task ID to cancel."`
	Reason []string `arg:"" optional:"" help:"Why the task is being canceled."`
}

func (c *boardTaskCancelCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	sess, err := c.resolveSession(ctx, d)
	if err != nil {
		return nil, err
	}
	return cl.board.CancelBoardTask(ctx, &pb.CancelBoardTaskRequest{
		Id:        c.ID,
		Reason:    strings.TrimSpace(strings.Join(c.Reason, " ")),
		SessionId: sess,
		AsProfile: c.As,
	})
}

// ---- Plain-text formatting helpers ----

func boardAuthorDisplay(m *pb.BoardMessage) string {
	if m.Author != nil && m.Author.Handle != "" {
		return m.Author.Handle
	}
	if m.By != nil && m.By.Actor != "" {
		return m.By.Actor
	}
	return "unknown"
}

func boardFirstLine(s string, maxLen int) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	if len(line) > maxLen {
		return line[:maxLen-1] + "…"
	}
	return line
}

func formatBoardLabelsInline(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, labels[k]))
	}
	return "  [" + strings.Join(parts, " ") + "]"
}

func formatBoardEventLine(ev *pb.BoardEvent) string {
	who := "-"
	if ev.Actor != nil && ev.Actor.Handle != "" {
		who = ev.Actor.Handle
	}
	ts := time.UnixMilli(ev.CreatedAt).Local().Format("15:04:05")
	summary := ""
	if ev.Message != nil {
		summary = "  " + boardFirstLine(ev.Message.Text, 80)
	}
	return fmt.Sprintf("@%-6d  %s  %-14s  %-18s  #%-6d  %s%s", ev.Cursor, ts, ev.Project, ev.Type, ev.MessageId, who, summary)
}

func formatBoardThreadsReport(res *pb.ListBoardThreadsResponse) string {
	var b strings.Builder
	for _, t := range res.Threads {
		m := t.Message
		if m == nil {
			continue
		}
		taskTag := ""
		if m.Task != nil {
			taskTag = fmt.Sprintf("  [%s]", boardTaskStateLabel(m.Task.State))
		}
		repTag := ""
		if t.Replies > 0 {
			repTag = fmt.Sprintf("  (%d replies)", t.Replies)
		}
		fmt.Fprintf(&b, "#%-6d  %-14s  %-18s%s  %s%s%s\n",
			m.Id, m.Project, boardAuthorDisplay(m), taskTag,
			boardFirstLine(m.Text, 80), repTag, formatBoardLabelsInline(m.Labels))
	}
	fmt.Fprintf(&b, "cursor=%d", res.Cursor)
	if res.Older != "" {
		fmt.Fprintf(&b, "  older=%s", res.Older)
	}
	b.WriteByte('\n')
	return b.String()
}

func formatBoardThreadReport(res *pb.GetBoardThreadResponse) string {
	var b strings.Builder
	writeMsg := func(m *pb.BoardMessage, indent string) {
		if m == nil {
			return
		}
		ts := time.UnixMilli(m.CreatedAt).Local().Format("2006-01-02 15:04")
		header := fmt.Sprintf("%s#%d  %s  %s  [%s]", indent, m.Id, boardAuthorDisplay(m), ts, m.Project)
		if m.Task != nil {
			header += fmt.Sprintf("  task=%s", boardTaskStateLabel(m.Task.State))
			if m.Task.ForActor != "" {
				header += " for=" + m.Task.ForActor
			}
			if len(m.Task.Needs) > 0 {
				needStrs := make([]string, len(m.Task.Needs))
				for i, n := range m.Task.Needs {
					needStrs[i] = fmt.Sprintf("#%d", n)
				}
				header += " needs=" + strings.Join(needStrs, ",")
			}
		}
		header += formatBoardLabelsInline(m.Labels)
		b.WriteString(header + "\n")
		if m.Deleted {
			b.WriteString(indent + "  (deleted)\n")
		} else {
			for _, line := range strings.Split(m.Text, "\n") {
				b.WriteString(indent + "  " + line + "\n")
			}
		}
		if len(m.Reactions) > 0 {
			var parts []string
			for _, r := range m.Reactions {
				parts = append(parts, fmt.Sprintf(":%s:×%d", r.Emoji, len(r.Actors)))
			}
			b.WriteString(indent + "  reactions: " + strings.Join(parts, " ") + "\n")
		}
		if len(m.Attachments) > 0 {
			var parts []string
			for _, a := range m.Attachments {
				parts = append(parts, fmt.Sprintf("%s (%s, %dB)", a.Name, a.Id, a.Size))
			}
			b.WriteString(indent + "  attachments: " + strings.Join(parts, ", ") + "\n")
		}
	}
	writeMsg(res.Message, "")
	for _, r := range res.Replies {
		b.WriteByte('\n')
		writeMsg(r, "  ")
	}
	if len(res.Schedules) > 0 {
		fmt.Fprintf(&b, "\nPending schedules (%d):\n", len(res.Schedules))
		for _, s := range res.Schedules {
			fmt.Fprintf(&b, "  sched#%d  next=%s  %s\n", s.Id, time.UnixMilli(s.NextAt).Local().Format(time.RFC3339), boardFirstLine(s.Text, 60))
		}
	}
	fmt.Fprintf(&b, "cursor=%d\n", res.Cursor)
	return b.String()
}

func formatBoardTasksReport(res *pb.ListBoardTasksResponse) string {
	var b strings.Builder
	for _, t := range res.Tasks {
		m := t.Message
		if m == nil || m.Task == nil {
			continue
		}
		extra := ""
		if m.Task.ForActor != "" {
			extra += "  for=" + m.Task.ForActor
		}
		if m.Task.Claim != nil {
			extra += "  claimed-by=" + m.Task.Claim.Actor
		}
		if m.Task.Blocked != nil && m.Task.Blocked.Reason != "" {
			extra += "  blocked=" + boardFirstLine(m.Task.Blocked.Reason, 40)
		}
		fmt.Fprintf(&b, "#%-6d  %-9s  %-14s  %s%s\n",
			m.Id, boardTaskStateLabel(m.Task.State), m.Project, boardFirstLine(m.Text, 72), extra)
	}
	fmt.Fprintf(&b, "cursor=%d\n", res.Cursor)
	return b.String()
}

func formatBoardEventsReport(res *pb.ListBoardEventsResponse) string {
	var b strings.Builder
	for _, ev := range res.Events {
		b.WriteString(formatBoardEventLine(ev) + "\n")
	}
	fmt.Fprintf(&b, "cursor=%d\n", res.Cursor)
	return b.String()
}
