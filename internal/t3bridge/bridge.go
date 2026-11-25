package t3bridge

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

const (
	DefaultDebounce        = 5 * time.Second
	DefaultPollInterval    = 3 * time.Second
	DefaultMaxErrorBackoff = 60 * time.Second
)

// EventKind identifies a WebSocket stream event delivered to the bridge state machine.
type EventKind int

const (
	EventSnapshot EventKind = iota + 1
	EventUpdate
	EventDisconnected
)

// Event is an input from the controller WebSocket stream into Bridge.
type Event struct {
	Kind      EventKind
	EventName string
	Questions []*pb.Question
}

// Config configures a Bridge instance.
type Config struct {
	ControllerURL   string
	TokenFunc       func(ctx context.Context) (string, error)
	HTTPClient      *http.Client
	T3              T3Client
	State           *StateStore
	Debounce        time.Duration
	PollInterval    time.Duration
	MaxErrorBackoff time.Duration
	PingInterval    time.Duration
	ReadTimeout     time.Duration
	Logger          *slog.Logger
	Now             func() time.Time
}

type threadWork struct {
	debounceUntil time.Time
	timer         *time.Timer
	timerSeq      uint64
	inFlight      bool
	ignored       bool
	errBackoff    time.Duration
}

type timerFire struct {
	threadID string
	seq      uint64
}

type workerResultKind int

const (
	workerResStatus workerResultKind = iota + 1
	workerResSent
)

type workerResult struct {
	kind     workerResultKind
	threadID string
	status   ThreadState
	batch    []*pb.Question
	err      error
}

// Bridge tracks Hotbed question outbox updates for T3 Code threads and delivers
// idle-aware, debounced, revision-tracked wake messages via the local T3 CLI.
type Bridge struct {
	cfg Config
	log *slog.Logger
	now func() time.Time

	events     chan Event
	timerFires chan timerFire
	workerDone chan workerResult
	done       chan struct{}
	doneOnce   sync.Once

	// Actor-owned state (accessed only inside RunLoop).
	connected bool
	outbox    map[int32]*pb.Question
	notified  map[int32]int64
	threads   map[string]*threadWork
	workerWG  sync.WaitGroup
}

// New constructs a Bridge and loads persisted notified revisions from cfg.State.
func New(cfg Config) (*Bridge, error) {
	if cfg.T3 == nil {
		return nil, errors.New("t3bridge: T3 client is required")
	}
	if cfg.Debounce <= 0 {
		cfg.Debounce = DefaultDebounce
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	if cfg.MaxErrorBackoff <= 0 {
		cfg.MaxErrorBackoff = DefaultMaxErrorBackoff
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}

	notified := map[int32]int64{}
	if cfg.State != nil {
		loaded, err := cfg.State.Load()
		if err != nil {
			return nil, err
		}
		notified = loaded
	}

	return &Bridge{
		cfg:        cfg,
		log:        log,
		now:        nowFn,
		events:     make(chan Event, 128),
		timerFires: make(chan timerFire, 128),
		workerDone: make(chan workerResult, 128),
		done:       make(chan struct{}),
		outbox:     map[int32]*pb.Question{},
		notified:   notified,
		threads:    map[string]*threadWork{},
	}, nil
}

// Enqueue injects an event into the bridge's actor loop (used by the WS loop and tests).
func (b *Bridge) Enqueue(ctx context.Context, ev Event) {
	select {
	case b.events <- ev:
	case <-ctx.Done():
	}
}

// Run connects outbound to the controller's /ws/questions stream and runs the
// bridge event loop until ctx is cancelled.
func (b *Bridge) Run(ctx context.Context) error {
	if b.cfg.ControllerURL == "" {
		return errors.New("t3bridge: controller URL is required")
	}
	if _, err := WSURL(b.cfg.ControllerURL); err != nil {
		return err
	}

	loopCtx, loopCancel := context.WithCancel(ctx)
	defer loopCancel()

	var wg sync.WaitGroup
	var wsErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		wsErr = runWSLoop(
			loopCtx,
			b.cfg.ControllerURL,
			b.cfg.TokenFunc,
			b.cfg.HTTPClient,
			b.cfg.PingInterval,
			b.cfg.ReadTimeout,
			func(ev Event) { b.Enqueue(loopCtx, ev) },
			b.log,
		)
		loopCancel()
	}()

	loopErr := b.RunLoop(loopCtx)
	loopCancel()
	wg.Wait()
	if wsErr != nil {
		return wsErr
	}
	return loopErr
}

// RunLoop executes the single-goroutine state machine until ctx is cancelled.
func (b *Bridge) RunLoop(ctx context.Context) error {
	defer b.shutdown()
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev := <-b.events:
			b.handleEvent(ctx, ev)
		case fire := <-b.timerFires:
			b.handleTimerFire(ctx, fire)
		case res := <-b.workerDone:
			b.handleWorkerResult(ctx, res)
		}
	}
}

func (b *Bridge) shutdown() {
	b.doneOnce.Do(func() { close(b.done) })
	for _, tw := range b.threads {
		if tw.timer != nil {
			tw.timer.Stop()
			tw.timer = nil
		}
	}
	b.workerWG.Wait()
}

func isOutboxState(s pb.QuestionState) bool {
	return s == pb.QuestionState_QUESTION_STATE_ANSWERED ||
		s == pb.QuestionState_QUESTION_STATE_DISMISSED
}

func (b *Bridge) handleEvent(ctx context.Context, ev Event) {
	switch ev.Kind {
	case EventDisconnected:
		b.connected = false
		for _, tw := range b.threads {
			if tw.timer != nil {
				tw.timer.Stop()
				tw.timer = nil
			}
		}

	case EventSnapshot:
		b.connected = true
		nextOutbox := make(map[int32]*pb.Question, len(ev.Questions))
		touchedThreads := map[string]bool{}
		for _, q := range ev.Questions {
			if q == nil || q.GetId() == 0 || q.GetT3CodeThreadId() == "" || !isOutboxState(q.GetState()) {
				continue
			}
			prev := b.outbox[q.GetId()]
			nextOutbox[q.GetId()] = q
			if prev == nil || prev.GetRevision() != q.GetRevision() || prev.GetState() != q.GetState() {
				touchedThreads[q.GetT3CodeThreadId()] = true
			} else if q.GetRevision() > b.notified[q.GetId()] {
				touchedThreads[q.GetT3CodeThreadId()] = true
			}
		}
		// Any thread that previously had pending questions no longer in the snapshot should be pruned.
		for _, oldQ := range b.outbox {
			if _, stillPresent := nextOutbox[oldQ.GetId()]; !stillPresent {
				touchedThreads[oldQ.GetT3CodeThreadId()] = true
			}
		}
		b.outbox = nextOutbox
		for tid := range touchedThreads {
			b.onThreadOutboxChanged(tid, true)
		}

	case EventUpdate:
		b.connected = true
		touchedThreads := map[string]bool{}
		newOrRevisedThreads := map[string]bool{}
		for _, q := range ev.Questions {
			if q == nil || q.GetId() == 0 {
				continue
			}
			oldQ := b.outbox[q.GetId()]
			if oldQ != nil && oldQ.GetT3CodeThreadId() != "" {
				touchedThreads[oldQ.GetT3CodeThreadId()] = true
			}
			tid := q.GetT3CodeThreadId()
			if tid != "" && isOutboxState(q.GetState()) {
				b.outbox[q.GetId()] = q
				touchedThreads[tid] = true
				if q.GetRevision() > b.notified[q.GetId()] {
					newOrRevisedThreads[tid] = true
				}
			} else {
				delete(b.outbox, q.GetId())
				if tid != "" {
					touchedThreads[tid] = true
				}
			}
		}
		for tid := range touchedThreads {
			b.onThreadOutboxChanged(tid, newOrRevisedThreads[tid])
		}
	}
	_ = ctx
}

func (b *Bridge) pendingForThread(threadID string) []*pb.Question {
	var out []*pb.Question
	for _, q := range b.outbox {
		if q.GetT3CodeThreadId() != threadID {
			continue
		}
		if !isOutboxState(q.GetState()) {
			continue
		}
		if q.GetRevision() <= b.notified[q.GetId()] {
			continue
		}
		out = append(out, q)
	}
	slices.SortFunc(out, func(a, c *pb.Question) int {
		return cmp.Compare(a.GetId(), c.GetId())
	})
	return out
}

func (b *Bridge) onThreadOutboxChanged(threadID string, resetDebounce bool) {
	if threadID == "" {
		return
	}
	pending := b.pendingForThread(threadID)
	if len(pending) == 0 {
		if tw, ok := b.threads[threadID]; ok {
			if tw.timer != nil {
				tw.timer.Stop()
				tw.timer = nil
			}
			if !tw.inFlight {
				delete(b.threads, threadID)
			}
		}
		return
	}
	tw := b.threads[threadID]
	if tw == nil {
		tw = &threadWork{}
		b.threads[threadID] = tw
	}
	if resetDebounce {
		// A new or revised answer clears any previous "not on this machine" marker
		// and extends the coalescing debounce window.
		tw.ignored = false
		tw.errBackoff = 0
		tw.debounceUntil = b.now().Add(b.cfg.Debounce)
	}
	if tw.ignored || !b.connected || tw.inFlight {
		return
	}
	delay := time.Until(tw.debounceUntil)
	if !b.cfg.NowIsDefault() {
		delay = tw.debounceUntil.Sub(b.now())
	}
	if delay < 0 {
		delay = 0
	}
	b.armTimer(threadID, tw, delay)
}

func (c Config) NowIsDefault() bool {
	return c.Now == nil
}

func (b *Bridge) armTimer(threadID string, tw *threadWork, delay time.Duration) {
	if tw.timer != nil {
		tw.timer.Stop()
	}
	tw.timerSeq++
	seq := tw.timerSeq
	tw.timer = time.AfterFunc(delay, func() {
		select {
		case b.timerFires <- timerFire{threadID: threadID, seq: seq}:
		case <-b.done:
		}
	})
}

func (b *Bridge) handleTimerFire(ctx context.Context, fire timerFire) {
	tw, ok := b.threads[fire.threadID]
	if !ok || tw.timerSeq != fire.seq {
		return
	}
	tw.timer = nil
	if !b.connected || tw.inFlight || tw.ignored {
		return
	}
	pending := b.pendingForThread(fire.threadID)
	if len(pending) == 0 {
		delete(b.threads, fire.threadID)
		return
	}
	if rem := tw.debounceUntil.Sub(b.now()); rem > 0 {
		b.armTimer(fire.threadID, tw, rem)
		return
	}
	tw.inFlight = true
	b.workerWG.Add(1)
	go func(tid string) {
		defer b.workerWG.Done()
		st, err := b.cfg.T3.ThreadStatus(ctx, tid)
		select {
		case b.workerDone <- workerResult{kind: workerResStatus, threadID: tid, status: st, err: err}:
		case <-ctx.Done():
		}
	}(fire.threadID)
}

func (b *Bridge) handleWorkerResult(ctx context.Context, res workerResult) {
	tw, ok := b.threads[res.threadID]
	if !ok {
		return
	}
	tw.inFlight = false

	switch res.kind {
	case workerResStatus:
		if res.err != nil {
			b.log.Warn("t3-bridge thread status check failed; will retry", "threadId", res.threadID, "err", res.err)
			if len(b.pendingForThread(res.threadID)) == 0 {
				delete(b.threads, res.threadID)
				return
			}
			if b.connected {
				b.armErrorBackoff(res.threadID, tw)
			}
			return
		}
		tw.errBackoff = 0
		if !res.status.Exists {
			b.log.Debug("t3-bridge skipping thread not present on this machine", "threadId", res.threadID)
			tw.ignored = true
			return
		}
		if res.status.IsBusy() {
			pending := b.pendingForThread(res.threadID)
			if len(pending) == 0 {
				delete(b.threads, res.threadID)
				return
			}
			if b.connected {
				b.log.Debug("t3-bridge thread busy; waiting for idle", "threadId", res.threadID, "status", res.status.Status, "pendingRequests", res.status.PendingRequestCount)
				b.armTimer(res.threadID, tw, b.cfg.PollInterval)
			}
			return
		}

		// Thread is idle. Re-check live outbox state & debounce window before sending.
		if !b.connected {
			return
		}
		pending := b.pendingForThread(res.threadID)
		if len(pending) == 0 {
			delete(b.threads, res.threadID)
			return
		}
		if rem := tw.debounceUntil.Sub(b.now()); rem > 0 {
			b.armTimer(res.threadID, tw, rem)
			return
		}

		msg := FormatWakeMessage(pending)
		tw.inFlight = true
		b.workerWG.Add(1)
		go func(tid string, batch []*pb.Question, text string) {
			defer b.workerWG.Done()
			err := b.cfg.T3.SendMessage(ctx, tid, text)
			select {
			case b.workerDone <- workerResult{kind: workerResSent, threadID: tid, batch: batch, err: err}:
			case <-ctx.Done():
			}
		}(res.threadID, pending, msg)

	case workerResSent:
		if res.err != nil {
			b.log.Warn("t3-bridge send wake message failed; will retry", "threadId", res.threadID, "err", res.err)
			if len(b.pendingForThread(res.threadID)) == 0 {
				delete(b.threads, res.threadID)
				return
			}
			if b.connected {
				b.armErrorBackoff(res.threadID, tw)
			}
			return
		}
		tw.errBackoff = 0
		ids := make([]int32, 0, len(res.batch))
		for _, q := range res.batch {
			ids = append(ids, q.GetId())
			if q.GetRevision() > b.notified[q.GetId()] {
				b.notified[q.GetId()] = q.GetRevision()
			}
		}
		if b.cfg.State != nil {
			if err := b.cfg.State.Save(b.notified); err != nil {
				b.log.Warn("t3-bridge persist notified state failed", "path", b.cfg.State.Path, "err", err)
			}
		}
		b.log.Info("t3-bridge notified idle thread", "threadId", res.threadID, "questionIds", ids)

		// If newer questions or revisions arrived while SendMessage was in flight,
		// wait for the thread to settle (at least pollInterval or remaining debounce).
		if remaining := b.pendingForThread(res.threadID); len(remaining) > 0 {
			if b.connected {
				wait := max(b.cfg.PollInterval, tw.debounceUntil.Sub(b.now()))
				b.armTimer(res.threadID, tw, wait)
			}
			return
		}
		delete(b.threads, res.threadID)
	}
}

func (b *Bridge) armErrorBackoff(threadID string, tw *threadWork) {
	if tw.errBackoff <= 0 {
		tw.errBackoff = b.cfg.PollInterval
	} else {
		tw.errBackoff = min(tw.errBackoff*2, b.cfg.MaxErrorBackoff)
	}
	b.armTimer(threadID, tw, tw.errBackoff)
}
