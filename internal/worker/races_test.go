package worker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/httpx"
	"github.com/semistrict/hotbed/internal/unit"
)

type hookStore struct {
	*fake.Store
	mu          sync.Mutex
	cloneCounts map[string]int
	onClone     func(ctx context.Context, snap, name string) error
	onSnapshot  func(ctx context.Context, vol, snap string) error
	onDestroy   func(ctx context.Context, name string) error
}

func newHookStore(root string) *hookStore {
	return &hookStore{
		Store:       &fake.Store{Root: root},
		cloneCounts: map[string]int{},
	}
}

func (s *hookStore) Clone(ctx context.Context, snap, name string) error {
	s.mu.Lock()
	s.cloneCounts[name]++
	fn := s.onClone
	s.mu.Unlock()
	if fn != nil {
		if err := fn(ctx, snap, name); err != nil {
			return err
		}
	}
	return s.Store.Clone(ctx, snap, name)
}

func (s *hookStore) Snapshot(ctx context.Context, vol, snap string) error {
	s.mu.Lock()
	fn := s.onSnapshot
	s.mu.Unlock()
	if fn != nil {
		if err := fn(ctx, vol, snap); err != nil {
			return err
		}
	}
	return s.Store.Snapshot(ctx, vol, snap)
}

func (s *hookStore) Destroy(ctx context.Context, name string) error {
	s.mu.Lock()
	fn := s.onDestroy
	s.mu.Unlock()
	if fn != nil {
		if err := fn(ctx, name); err != nil {
			return err
		}
	}
	return s.Store.Destroy(ctx, name)
}

func (s *hookStore) cloneCount(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cloneCounts[name]
}

type raceHookUnits struct {
	*fake.Units
	onStatus func(ctx context.Context, name string)
	onStart  func(ctx context.Context, spec unit.Spec) error
	onStop   func(ctx context.Context, name string) error
}

func (u *raceHookUnits) Status(ctx context.Context, name string) (unit.State, error) {
	if u.onStatus != nil {
		u.onStatus(ctx, name)
	}
	return u.Units.Status(ctx, name)
}

func (u *raceHookUnits) StatusMany(ctx context.Context, names []string) (map[string]unit.State, error) {
	if u.onStatus != nil {
		for _, name := range names {
			u.onStatus(ctx, name)
		}
	}
	return u.Units.StatusMany(ctx, names)
}

func (u *raceHookUnits) Start(ctx context.Context, spec unit.Spec) error {
	if u.onStart != nil {
		if err := u.onStart(ctx, spec); err != nil {
			return err
		}
	}
	return u.Units.Start(ctx, spec)
}

func (u *raceHookUnits) Stop(ctx context.Context, name string) error {
	if u.onStop != nil {
		if err := u.onStop(ctx, name); err != nil {
			return err
		}
	}
	return u.Units.Stop(ctx, name)
}

func testOriginRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"commit", "-qm", "Initial commit"}} {
		if args[0] == "add" {
			if err := os.WriteFile(filepath.Join(dir, "lib.txt"), []byte("base\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

func startTestEngine(t *testing.T, cfg EngineConfig) *Engine {
	t.Helper()
	if cfg.Dir == "" {
		cfg.Dir = t.TempDir()
	}
	if cfg.NodeID == "" {
		cfg.NodeID = "n1"
	}
	if cfg.Host == "" {
		cfg.Host = "worker1"
	}
	if cfg.Store == nil {
		cfg.Store = &fake.Store{Root: filepath.Join(cfg.Dir, "volumes")}
	}
	if cfg.BaseImage == nil {
		cfg.BaseImage = fake.BaseImage(cfg.Store)
	}
	if cfg.Units == nil {
		cfg.Units = &fake.Units{}
	}
	if cfg.System == nil {
		cfg.System = fake.NewSystem()
	}
	cfg.AllowLocalRepos = true
	eng, err := NewEngine(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { eng.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
		eng.Close()
	})
	return eng
}

// Concurrent CreateWorkspace calls with the same ID race across baseFor()
// (which unlocks e.mu for git ls-remote). Only one may succeed; all others
// must fail with CodeConflict without overwriting e.workspaces[id] (board 7886).
func TestCreateWorkspaceDuplicateIDRace(t *testing.T) {
	eng := startTestEngine(t, EngineConfig{})
	project := &pb.HostedProject{
		Name:  "app",
		Repos: []*pb.Repo{{Id: "app", Url: testOriginRepo(t)}},
		Steps: []*pb.Step{{Name: "build", Script: "true", EveryPull: true, Hash: "h1"}},
	}

	const n = 8
	start := make(chan struct{})
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = eng.CreateWorkspace(t.Context(), &pb.CreateWorkspaceRequest{
				Id: "dup-ws", Repo: "app", SessionId: "s1", Agent: "a", Project: project,
			})
		}(i)
	}
	close(start)
	wg.Wait()

	var okCount, conflictCount int
	for _, err := range errs {
		switch {
		case err == nil:
			okCount++
		case api.CodeOf(err) == api.CodeConflict:
			conflictCount++
		default:
			t.Errorf("unexpected CreateWorkspace error: %v", err)
		}
	}
	if okCount != 1 || conflictCount != n-1 {
		t.Fatalf("got %d successes and %d conflicts, want 1 and %d", okCount, conflictCount, n-1)
	}

	eventuallyTrue(t, "dup-ws ready", func() bool {
		ws, err := eng.Workspace("dup-ws")
		return err == nil && ws.State == idle
	})

	events, err := eng.Events(0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var created int
	for _, ev := range events {
		if wc := ev.GetWorkspace(); wc != nil && wc.Workspace.Id == "dup-ws" && wc.What == "created" {
			created++
		}
	}
	if created != 1 {
		t.Errorf("got %d created events for dup-ws, want 1", created)
	}
}

// Concurrent fork() and forkPrepared() calls for the same workspace (as when
// freshened() finishes while CreateWorkspace is inserting the workspace) must
// only invoke Store.Clone once and leave the workspace IDLE, never FAILED
// (board 7886).
func TestCreateWorkspaceDoubleForkRace(t *testing.T) {
	dir := t.TempDir()
	store := newHookStore(filepath.Join(dir, "volumes"))
	store.onClone = func(ctx context.Context, snap, name string) error {
		// Widen the race window while Clone is in flight.
		time.Sleep(15 * time.Millisecond)
		return nil
	}
	eng := startTestEngine(t, EngineConfig{Dir: dir, Store: store})
	project := &pb.HostedProject{
		Name:  "app",
		Repos: []*pb.Repo{{Id: "app", Url: testOriginRepo(t)}},
		Steps: []*pb.Step{{Name: "build", Script: "true", EveryPull: true, Hash: "h1"}},
	}

	// Build a warm base first.
	if _, err := eng.CreateWorkspace(t.Context(), &pb.CreateWorkspaceRequest{
		Id: "seed", Repo: "app", SessionId: "s0", Agent: "a", Project: project,
	}); err != nil {
		t.Fatal(err)
	}
	eventuallyTrue(t, "seed workspace ready", func() bool {
		ws, err := eng.Workspace("seed")
		return err == nil && ws.State == idle
	})

	for i := range 5 {
		wsID := "double-fork-" + string(rune('a'+i))
		w := &workspace{rec: &pb.WorkspaceRecord{
			Workspace: &pb.Workspace{
				Id: wsID, Project: "app", BaseId: "app-g1", NodeId: "n1", Agent: "a",
				Repos: []*pb.WorkspaceRepo{{Repo: "app"}}, State: preparing,
			},
			Volume: "ws/" + wsID, BaseSnapshot: "app-g1",
		}}
		eng.mu.Lock()
		eng.workspaces[wsID] = w
		eng.saveWorkspace(w, change{what: "created"})
		eng.mu.Unlock()

		// Race CreateWorkspace's e.fork(ctx, w) against freshened()'s e.forkPrepared("app-g1").
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(3)
		var forkErr error
		go func() {
			defer wg.Done()
			<-start
			forkErr = eng.fork(t.Context(), w)
		}()
		go func() {
			defer wg.Done()
			<-start
			eng.forkPrepared(t.Context(), "app-g1")
		}()
		go func() {
			defer wg.Done()
			<-start
			eng.forkPrepared(t.Context(), "app-g1")
		}()
		close(start)
		wg.Wait()

		if forkErr != nil {
			t.Fatalf("%s: fork returned error: %v", wsID, forkErr)
		}
		if got := store.cloneCount("ws/" + wsID); got != 1 {
			t.Fatalf("%s: Store.Clone called %d times, want 1", wsID, got)
		}
		ws, err := eng.Workspace(wsID)
		if err != nil {
			t.Fatal(err)
		}
		if ws.State != idle {
			t.Fatalf("%s: state = %v (reason: %s), want IDLE", wsID, ws.State, ws.Reason)
		}
	}
}

// Sync and ReleaseWorkspace must serialize volume access via w.sync, read
// workspace state/volume under e.mu without data races, never emit "synced"
// after release, and concurrent ReleaseWorkspace calls must emit at most one
// "released" event (board 7886).
func TestSyncVsReleaseWorkspaceRace(t *testing.T) {
	dir := t.TempDir()
	store := newHookStore(filepath.Join(dir, "volumes"))
	var destroyed atomic.Bool
	store.onDestroy = func(ctx context.Context, name string) error {
		if strings.HasPrefix(name, "ws/") {
			destroyed.Store(true)
		}
		return nil
	}
	eng := startTestEngine(t, EngineConfig{Dir: dir, Store: store})
	project := &pb.HostedProject{
		Name:  "app",
		Repos: []*pb.Repo{{Id: "app", Url: testOriginRepo(t)}},
		Steps: []*pb.Step{{Name: "build", Script: "true", EveryPull: true, Hash: "h1"}},
	}
	if _, err := eng.CreateWorkspace(t.Context(), &pb.CreateWorkspaceRequest{
		Id: "ws1", Repo: "app", SessionId: "s1", Agent: "a", Project: project,
	}); err != nil {
		t.Fatal(err)
	}
	eventuallyTrue(t, "ws1 ready", func() bool {
		ws, err := eng.Workspace("ws1")
		return err == nil && ws.State == idle
	})

	// Use a pipe so Sync enters and holds w.sync while ReleaseWorkspace runs.
	pr, pw := io.Pipe()
	syncStarted := make(chan struct{})
	syncDone := make(chan error, 1)
	go func() {
		_, err := eng.Sync(t.Context(), &pb.SyncHeader{
			WorkspaceId: "ws1",
			Repo:        "app",
			Tree:        "t1",
		}, &notifyReader{r: pr, onFirstRead: func() { close(syncStarted) }})
		syncDone <- err
	}()
	<-syncStarted

	// Launch multiple concurrent ReleaseWorkspace calls while Sync holds w.sync.
	const relCalls = 4
	relErrs := make([]error, relCalls)
	var wg sync.WaitGroup
	for i := range relCalls {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			relErrs[i] = eng.ReleaseWorkspace(t.Context(), "ws1")
		}(i)
	}

	// Wait briefly to ensure ReleaseWorkspace has set w.releasing and is waiting
	// on w.sync without having destroyed the volume yet.
	eventuallyTrue(t, "ws1 releasing", func() bool {
		eng.mu.Lock()
		defer eng.mu.Unlock()
		return eng.workspaces["ws1"].releasing
	})
	if destroyed.Load() {
		t.Fatal("ReleaseWorkspace destroyed the volume while Sync still held w.sync")
	}

	// Let Sync finish with an empty bundle; it must notice w.releasing and NOT
	// emit a "synced" event.
	_ = pw.Close()
	syncErr := <-syncDone
	wg.Wait()

	if !destroyed.Load() {
		t.Fatal("ReleaseWorkspace never destroyed the volume after Sync released w.sync")
	}
	if api.CodeOf(syncErr) != api.CodeNotFound {
		t.Errorf("Sync racing ReleaseWorkspace = %v, want CodeNotFound", syncErr)
	}

	var relOK, relNotFound int
	for _, err := range relErrs {
		switch {
		case err == nil:
			relOK++
		case api.CodeOf(err) == api.CodeNotFound:
			relNotFound++
		default:
			t.Errorf("unexpected ReleaseWorkspace error: %v", err)
		}
	}
	if relOK != 1 || relNotFound != relCalls-1 {
		t.Fatalf("concurrent ReleaseWorkspace: got %d ok and %d not-found, want 1 and %d", relOK, relNotFound, relCalls-1)
	}

	events, err := eng.Events(0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var releasedEvents, syncedEvents int
	for _, ev := range events {
		if wc := ev.GetWorkspace(); wc != nil && wc.Workspace.Id == "ws1" {
			if wc.What == "released" {
				releasedEvents++
			}
			if wc.What == "synced" {
				syncedEvents++
			}
		}
	}
	if releasedEvents != 1 {
		t.Errorf("got %d released events, want 1", releasedEvents)
	}
	if syncedEvents != 0 {
		t.Errorf("got %d synced events after release started, want 0", syncedEvents)
	}
}

type notifyReader struct {
	r           io.Reader
	once        sync.Once
	onFirstRead func()
}

func (n *notifyReader) Read(p []byte) (int, error) {
	n.once.Do(n.onFirstRead)
	return n.r.Read(p)
}

// Stop racing with admit (after makeTmp creates .tmp/<id>, before or during
// Units.Start) must clean up the freshly created tmpDir instead of leaking it
// (board 7886).
func TestStopVsAdmitTmpDirRace(t *testing.T) {
	t.Run("stop during Units.Start cleans tmpDir", func(t *testing.T) {
		dir := t.TempDir()
		store := &fake.Store{Root: filepath.Join(dir, "volumes")}
		inStart := make(chan struct{})
		releaseStart := make(chan struct{})
		units := &raceHookUnits{
			Units: &fake.Units{},
			onStart: func(ctx context.Context, spec unit.Spec) error {
				if spec.Name == "hotbed-race-cmd" {
					close(inStart)
					<-releaseStart
				}
				return nil
			},
		}
		eng, err := NewEngine(t.Context(), EngineConfig{
			Dir: dir, NodeID: "n1", Host: "worker1",
			Store: store, BaseImage: fake.BaseImage(store), Units: units, System: fake.NewSystem(),
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { eng.Close() })
		if err := store.Create(t.Context(), "ws/w1"); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(store.Path("ws/w1"), "work", "app"), 0o755); err != nil {
			t.Fatal(err)
		}
		eng.mu.Lock()
		eng.workspaces["w1"] = &workspace{rec: &pb.WorkspaceRecord{
			Workspace: &pb.Workspace{Id: "w1", Project: "app", State: idle, Repos: []*pb.WorkspaceRepo{{Repo: "app"}}},
			Volume:    "ws/w1",
		}}
		eng.mu.Unlock()

		if _, err := eng.Exec(t.Context(), &pb.RunCommandRequest{
			Id: "race-cmd", WorkspaceId: "w1", Args: []string{"sleep", "60"},
		}); err != nil {
			t.Fatal(err)
		}

		admitDone := make(chan struct{})
		go func() {
			defer close(admitDone)
			eng.admit(t.Context())
		}()

		<-inStart
		// tmpDir now exists on disk, and command is still in state=queued while
		// admit is inside Units.Start.
		tmpPath := filepath.Join(store.Path("ws/w1"), tmpName, "race-cmd")
		if _, err := os.Stat(tmpPath); err != nil {
			t.Fatalf("expected tmpDir %s to exist during admit: %v", tmpPath, err)
		}

		stopDone := make(chan error, 1)
		go func() {
			_, err := eng.Stop(t.Context(), "race-cmd", "cancelled during admit")
			stopDone <- err
		}()
		// Wait until Stop has signalled c.stopping under e.mu.
		eventuallyTrue(t, "race-cmd stopping", func() bool {
			eng.mu.Lock()
			defer eng.mu.Unlock()
			c, ok := eng.commands["race-cmd"]
			return ok && c.stopping
		})
		close(releaseStart)

		if err := <-stopDone; err != nil {
			t.Fatalf("Stop returned error: %v", err)
		}
		<-admitDone

		c, ok := eng.Command("race-cmd")
		if !ok || c.State != cancelled {
			t.Fatalf("race-cmd state = %v (ok=%v), want CANCELLED", c.GetState(), ok)
		}

		// Once Stop returns, the unit must be stopped and tmpDir must be gone.
		if _, err := os.Stat(tmpPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("tmpDir %s leaked after Stop vs admit race: err=%v", tmpPath, err)
		}
	})

	t.Run("stop racing ahead of makeTmp cleans tmpDir", func(t *testing.T) {
		dir := t.TempDir()
		store := &fake.Store{Root: filepath.Join(dir, "volumes")}
		eng, err := NewEngine(t.Context(), EngineConfig{
			Dir: dir, NodeID: "n1", Host: "worker1",
			Store: store, BaseImage: fake.BaseImage(store), Units: &fake.Units{}, System: fake.NewSystem(),
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { eng.Close() })
		if err := store.Create(t.Context(), "ws/w1"); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(store.Path("ws/w1"), "work", "app"), 0o755); err != nil {
			t.Fatal(err)
		}
		eng.mu.Lock()
		eng.workspaces["w1"] = &workspace{rec: &pb.WorkspaceRecord{
			Workspace: &pb.Workspace{Id: "w1", Project: "app", State: idle, Repos: []*pb.WorkspaceRepo{{Repo: "app"}}},
			Volume:    "ws/w1",
		}}
		eng.mu.Unlock()

		for i := range 20 {
			cmdID := "stop-race-" + string(rune('a'+i))
			if _, err := eng.Exec(t.Context(), &pb.RunCommandRequest{
				Id: cmdID, WorkspaceId: "w1", Args: []string{"sleep", "60"},
			}); err != nil {
				t.Fatal(err)
			}
			start := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				eng.admit(t.Context())
			}()
			go func() {
				defer wg.Done()
				<-start
				_, _ = eng.Stop(t.Context(), cmdID, "race stop")
			}()
			close(start)
			wg.Wait()

			if err := eng.tmpsGone(t.Context(), "w1"); err != nil {
				t.Fatal(err)
			}
			tmpPath := filepath.Join(store.Path("ws/w1"), tmpName, cmdID)
			if _, err := os.Stat(tmpPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("iteration %d: tmpDir %s leaked after Stop vs admit race: err=%v", i, tmpPath, err)
			}
		}
	})
}

// Engine.freshened() must NOT hold e.mu across Store.Snapshot or git log, and
// must keep base.State == BUILDING until Snapshot completes so CreateWorkspace
// cannot race cloning an unfinished snapshot (board 8013).
func TestFreshenedDoesNotHoldEngineLockDuringSnapshot(t *testing.T) {
	dir := t.TempDir()
	store := newHookStore(filepath.Join(dir, "volumes"))
	snapStarted := make(chan struct{})
	releaseSnap := make(chan struct{})
	var snapOnce sync.Once
	var snapFinished atomic.Bool
	var clonedBeforeSnap atomic.Bool

	store.onSnapshot = func(ctx context.Context, vol, snap string) error {
		if !strings.HasPrefix(vol, "bases/") {
			return nil
		}
		snapOnce.Do(func() {
			close(snapStarted)
			<-releaseSnap
		})
		return nil
	}
	store.onClone = func(ctx context.Context, snap, name string) error {
		if strings.HasPrefix(name, "ws/") && !snapFinished.Load() {
			clonedBeforeSnap.Store(true)
		}
		return nil
	}

	eng := startTestEngine(t, EngineConfig{Dir: dir, Store: store})
	project := &pb.HostedProject{
		Name:  "app",
		Repos: []*pb.Repo{{Id: "app", Url: testOriginRepo(t)}},
		Steps: []*pb.Step{{Name: "build", Script: "true", EveryPull: true, Hash: "h1"}},
	}

	if _, err := eng.CreateWorkspace(t.Context(), &pb.CreateWorkspaceRequest{
		Id: "ws1", Repo: "app", SessionId: "s1", Agent: "a", Project: project,
	}); err != nil {
		t.Fatal(err)
	}

	// Wait until freshened() is blocked inside Store.Snapshot.
	select {
	case <-snapStarted:
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for Store.Snapshot to start")
	}

	// While Store.Snapshot is blocked, e.mu must NOT be held: Workspaces(),
	// Heartbeat(), and CreateWorkspace() must all complete immediately without
	// stalling, and ws2 must remain PREPARING (not cloning an unfinished snapshot).
	rpcDone := make(chan struct{})
	go func() {
		defer close(rpcDone)
		if wsList := eng.Workspaces(); len(wsList) != 1 {
			t.Errorf("Workspaces() = %d, want 1", len(wsList))
		}
		if hb := eng.Heartbeat("cluster"); hb == nil {
			t.Error("Heartbeat returned nil")
		}
		ws2, err := eng.CreateWorkspace(t.Context(), &pb.CreateWorkspaceRequest{
			Id: "ws2", Repo: "app", SessionId: "s2", Agent: "b", Project: project,
		})
		if err != nil {
			t.Errorf("CreateWorkspace(ws2) during slow snapshot: %v", err)
			return
		}
		if ws2.State != preparing {
			t.Errorf("ws2 state during slow snapshot = %v, want PREPARING", ws2.State)
		}
	}()

	select {
	case <-rpcDone:
	case <-time.After(2 * time.Second):
		close(releaseSnap)
		t.Fatal("worker RPCs blocked while Store.Snapshot was in progress (e.mu held across Snapshot)")
	}

	if clonedBeforeSnap.Load() {
		close(releaseSnap)
		t.Fatal("Store.Clone was called before Store.Snapshot completed")
	}

	snapFinished.Store(true)
	close(releaseSnap)

	eventuallyTrue(t, "both workspaces ready after snapshot finishes", func() bool {
		w1, e1 := eng.Workspace("ws1")
		w2, e2 := eng.Workspace("ws2")
		return e1 == nil && e2 == nil && w1.State == idle && w2.State == idle
	})

	// Verify git log -1 metadata populated on the warm base.
	eng.mu.Lock()
	b := eng.bases["app-g1"]
	var msg string
	if b != nil && len(b.Repos) > 0 {
		msg = b.Repos[0].Message
	}
	eng.mu.Unlock()
	if msg != "Initial commit" {
		t.Errorf("base repo commit message = %q, want %q", msg, "Initial commit")
	}
}

// FollowLog keeps its open log file descriptor across appends, wakes promptly
// as output arrives, and drains trailing bytes cleanly when the command ends
// (board 8013).
func TestFollowLogStreamsGrowingLogAndDrainsOnExit(t *testing.T) {
	eng, _ := outputEngine(t, DefaultLogLimit)
	// Seed a live command whose log file does not exist yet (queued).
	eng.mu.Lock()
	c := eng.newCommand(&pb.CommandRecord{
		Command: &pb.Command{Id: "follow-1", WorkspaceId: "w", State: queued},
	})
	eng.commands["follow-1"] = c
	eng.mu.Unlock()

	client := clientAs(t, httpx.Peer{NodeID: "nClient", Hostname: "laptop", Login: "ramon@"}, eng)

	// Call FollowLog in a goroutine: HTTP response headers are flushed on the
	// first chunk, which arrives once the queued command starts writing its log.
	var received bytes.Buffer
	recvDone := make(chan error, 1)
	go func() {
		stream, err := client.FollowLog(t.Context(), &pb.FollowLogRequest{Id: "follow-1", Follow: true})
		if err != nil {
			recvDone <- err
			return
		}
		defer stream.Close()
		for {
			chunk, err := stream.Receive()
			if errors.Is(err, io.EOF) {
				recvDone <- nil
				return
			}
			if err != nil {
				recvDone <- err
				return
			}
			received.Write(chunk.Data)
		}
	}()

	// Wait until the follower is registered.
	eventuallyTrue(t, "follower registered", func() bool {
		select {
		case err := <-recvDone:
			t.Fatalf("FollowLog stream exited early: %v", err)
		default:
		}
		eng.mu.Lock()
		defer eng.mu.Unlock()
		return c.followers == 1
	})

	logPath, err := eng.logPath("follow-1")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("line 1\n"); err != nil {
		t.Fatal(err)
	}
	eng.mu.Lock()
	c.cmd().State = running
	eng.notify()
	eng.mu.Unlock()

	time.Sleep(40 * time.Millisecond)
	if _, err := f.WriteString("line 2\n"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)
	if _, err := f.WriteString("line 3\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	eng.mu.Lock()
	zero := int32(0)
	eng.end(c, succeeded, &zero, "")
	eng.mu.Unlock()

	select {
	case err := <-recvDone:
		if err != nil {
			t.Fatalf("FollowLog stream error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for FollowLog to close after command ended")
	}

	want := "line 1\nline 2\nline 3\n"
	if got := received.String(); got != want {
		t.Errorf("FollowLog received %q, want %q", got, want)
	}
}

// Regression test for issues #20/#21: while a followed command is queued and
// produces no output, FollowLog emits periodic empty keepalive chunks so edge
// proxies keep the stream open and clients can detect silent stalls.
func TestFollowLogEmitsKeepaliveWhileQueued(t *testing.T) {
	eng, _ := outputEngine(t, DefaultLogLimit)
	eng.cfg.StreamKeepalive = 30 * time.Millisecond
	eng.mu.Lock()
	c := eng.newCommand(&pb.CommandRecord{
		Command: &pb.Command{
			Id:          "follow-keepalive",
			WorkspaceId: "w",
			State:       queued,
			Attached:    true,
		},
	})
	eng.commands[c.cmd().Id] = c
	eng.mu.Unlock()

	client := clientAs(t, httpx.Peer{NodeID: "ctrl", Hostname: "ctrl", Login: "owner"}, eng)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	stream, err := client.FollowLog(ctx, &pb.FollowLogRequest{Id: "follow-keepalive", Follow: true})
	if err != nil {
		t.Fatalf("FollowLog: %v", err)
	}
	defer stream.Close()

	keepalives := 0
	for keepalives < 2 {
		chunk, err := stream.Receive()
		if err != nil {
			t.Fatalf("Receive after %d keepalives: %v", keepalives, err)
		}
		if len(chunk.Data) != 0 || chunk.NextOffset != 0 {
			t.Fatalf("unexpected chunk %+v, want empty keepalive at offset 0", chunk)
		}
		keepalives++
	}
}

// Engine.Sync must enforce MaxSyncBundleBytes across submodule and repo bundles,
// rejecting oversized uploads with CodeInvalidArgs and leaving no temp bundle
// files behind (board 8015).
func TestSyncRejectsOversizedBundle(t *testing.T) {
	dir := t.TempDir()
	eng := startTestEngine(t, EngineConfig{
		Dir:                dir,
		MaxSyncBundleBytes: 1024,
	})
	project := &pb.HostedProject{
		Name:  "app",
		Repos: []*pb.Repo{{Id: "app", Url: testOriginRepo(t)}},
		Steps: []*pb.Step{{Name: "build", Script: "true", EveryPull: true, Hash: "h1"}},
	}
	if _, err := eng.CreateWorkspace(t.Context(), &pb.CreateWorkspaceRequest{
		Id: "ws1", Repo: "app", SessionId: "s1", Agent: "a", Project: project,
	}); err != nil {
		t.Fatal(err)
	}
	eventuallyTrue(t, "ws1 ready", func() bool {
		ws, err := eng.Workspace("ws1")
		return err == nil && ws.State == idle
	})

	bundlesDir := filepath.Join(dir, "bundles")
	assertNoTempBundles := func(t *testing.T) {
		t.Helper()
		entries, err := os.ReadDir(bundlesDir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Errorf("bundles dir has leftover temp files: %v", entries)
		}
	}

	// 1. Oversized main repo bundle (> 1024 bytes).
	_, err := eng.Sync(t.Context(), &pb.SyncHeader{
		WorkspaceId: "ws1", Repo: "app", Tree: "big-repo",
	}, bytes.NewReader(bytes.Repeat([]byte("a"), 2048)))
	if api.CodeOf(err) != api.CodeInvalidArgs || !strings.Contains(err.Error(), "maximum size") {
		t.Fatalf("oversized repo bundle err = %v, want CodeInvalidArgs mentioning maximum size", err)
	}
	assertNoTempBundles(t)

	// 2. Oversized submodule bundle declared in header (> 1024 bytes).
	_, err = eng.Sync(t.Context(), &pb.SyncHeader{
		WorkspaceId: "ws1", Repo: "app", Tree: "big-sub",
		Submodules: []*pb.SyncSubmodule{{Path: "sub", Commit: "deadbeef", BundleBytes: 2048}},
	}, bytes.NewReader(bytes.Repeat([]byte("b"), 2048)))
	if api.CodeOf(err) != api.CodeInvalidArgs || !strings.Contains(err.Error(), "maximum size") {
		t.Fatalf("oversized submodule bundle err = %v, want CodeInvalidArgs mentioning maximum size", err)
	}
	assertNoTempBundles(t)

	// 3. Combined submodule + main repo bundle exceeding 1024 bytes.
	_, err = eng.Sync(t.Context(), &pb.SyncHeader{
		WorkspaceId: "ws1", Repo: "app", Tree: "combined-big",
		Submodules: []*pb.SyncSubmodule{{Path: "sub", Commit: "deadbeef", BundleBytes: 600}},
	}, bytes.NewReader(bytes.Repeat([]byte("c"), 1200)))
	if api.CodeOf(err) != api.CodeInvalidArgs || !strings.Contains(err.Error(), "maximum size") {
		t.Fatalf("combined oversized bundle err = %v, want CodeInvalidArgs mentioning maximum size", err)
	}
	assertNoTempBundles(t)

	// 4. Negative submodule bundle size rejected cleanly.
	_, err = eng.Sync(t.Context(), &pb.SyncHeader{
		WorkspaceId: "ws1", Repo: "app", Tree: "neg-sub",
		Submodules: []*pb.SyncSubmodule{{Path: "sub", Commit: "deadbeef", BundleBytes: -1}},
	}, bytes.NewReader(nil))
	if api.CodeOf(err) != api.CodeInvalidArgs {
		t.Fatalf("negative submodule bundle size err = %v, want CodeInvalidArgs", err)
	}
	assertNoTempBundles(t)

	// 5. Empty bundle within limit succeeds.
	resp, err := eng.Sync(t.Context(), &pb.SyncHeader{
		WorkspaceId: "ws1", Repo: "app", Tree: "ok-empty",
	}, bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("empty sync within limit failed: %v", err)
	}
	if resp.WorkspaceId != "ws1" || resp.BundleBytes != 0 {
		t.Errorf("unexpected sync response: %+v", resp)
	}
	assertNoTempBundles(t)

	// 6. Equal-tree skipped sync exceeding MaxSyncBundleBytes is rejected
	// without draining unbounded data (Finding #86, board 8289).
	cr := &countingInfiniteReader{}
	_, err = eng.Sync(t.Context(), &pb.SyncHeader{
		WorkspaceId: "ws1", Repo: "app", Tree: "ok-empty",
	}, cr)
	if api.CodeOf(err) != api.CodeInvalidArgs || !strings.Contains(err.Error(), "maximum size") {
		t.Fatalf("oversized skipped sync err = %v, want CodeInvalidArgs mentioning maximum size", err)
	}
	if cr.n > 1024+1 {
		t.Fatalf("oversized skipped sync read %d bytes, want at most %d", cr.n, 1024+1)
	}
	assertNoTempBundles(t)

	// 7. Equal-tree skipped sync within limit succeeds with Skipped=true.
	resp, err = eng.Sync(t.Context(), &pb.SyncHeader{
		WorkspaceId: "ws1", Repo: "app", Tree: "ok-empty",
	}, bytes.NewReader(bytes.Repeat([]byte("x"), 128)))
	if err != nil {
		t.Fatalf("skipped sync within limit failed: %v", err)
	}
	if !resp.Skipped || resp.Tree != "ok-empty" {
		t.Errorf("unexpected skipped sync response: %+v", resp)
	}
	assertNoTempBundles(t)
}

type countingInfiniteReader struct {
	n int64
}

func (r *countingInfiniteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'z'
	}
	r.n += int64(len(p))
	return len(p), nil
}

// Finding #78 (board 8291): a concurrent ReleaseWorkspace must wait for the
// in-flight teardown to finish and propagate its failure rather than returning
// CodeNotFound prematurely.
func TestConcurrentReleaseWorkspaceWaitsAndPropagatesFailure(t *testing.T) {
	dir := t.TempDir()
	store := newHookStore(filepath.Join(dir, "volumes"))
	inDestroy := make(chan struct{})
	releaseDestroy := make(chan struct{})
	errBoom := errors.New("btrfs destroy I/O error")
	var failDestroy atomic.Bool
	failDestroy.Store(true)
	var destroyOnce sync.Once
	store.onDestroy = func(ctx context.Context, name string) error {
		if name == "ws/ws1" && failDestroy.Load() {
			destroyOnce.Do(func() {
				close(inDestroy)
				<-releaseDestroy
			})
			return errBoom
		}
		return nil
	}
	eng := startTestEngine(t, EngineConfig{Dir: dir, Store: store})
	project := &pb.HostedProject{
		Name:  "app",
		Repos: []*pb.Repo{{Id: "app", Url: testOriginRepo(t)}},
		Steps: []*pb.Step{{Name: "build", Script: "true", EveryPull: true, Hash: "h1"}},
	}
	if _, err := eng.CreateWorkspace(t.Context(), &pb.CreateWorkspaceRequest{
		Id: "ws1", Repo: "app", SessionId: "s1", Agent: "a", Project: project,
	}); err != nil {
		t.Fatal(err)
	}
	eventuallyTrue(t, "ws1 ready", func() bool {
		ws, err := eng.Workspace("ws1")
		return err == nil && ws.State == idle
	})

	rel1Done := make(chan error, 1)
	go func() {
		rel1Done <- eng.ReleaseWorkspace(t.Context(), "ws1")
	}()
	<-inDestroy

	rel2Done := make(chan error, 1)
	go func() {
		rel2Done <- eng.ReleaseWorkspace(t.Context(), "ws1")
	}()

	// Second caller must wait while the first ReleaseWorkspace is still in flight.
	select {
	case err := <-rel2Done:
		t.Fatalf("second ReleaseWorkspace returned prematurely before teardown finished: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseDestroy)
	err1 := <-rel1Done
	err2 := <-rel2Done

	if !errors.Is(err1, errBoom) {
		t.Fatalf("first ReleaseWorkspace err = %v, want %v", err1, errBoom)
	}
	if !errors.Is(err2, errBoom) {
		t.Fatalf("second ReleaseWorkspace err = %v, want %v (must not return CodeNotFound/nil on failed teardown)", err2, errBoom)
	}
	if api.CodeOf(err2) == api.CodeNotFound {
		t.Fatalf("second ReleaseWorkspace returned CodeNotFound despite failed teardown")
	}

	// Workspace must still be live (idle) and releasable once Destroy succeeds.
	ws, err := eng.Workspace("ws1")
	if err != nil {
		t.Fatal(err)
	}
	if ws.State != idle {
		t.Fatalf("ws1 state after failed release = %v, want IDLE", ws.State)
	}

	failDestroy.Store(false)
	if err := eng.ReleaseWorkspace(t.Context(), "ws1"); err != nil {
		t.Fatalf("retry ReleaseWorkspace failed: %v", err)
	}
	ws, err = eng.Workspace("ws1")
	if err != nil {
		t.Fatal(err)
	}
	if ws.State != released {
		t.Fatalf("ws1 state after retry release = %v, want RELEASED", ws.State)
	}
}

// Finding #79 (board 8291): if Store.Clone finishes while ReleaseWorkspace is
// waiting on forkDone, and ReleaseWorkspace then fails (e.g. Destroy error),
// the workspace must NOT be stranded in PREPARING.
func TestFailedReleaseDoesNotStrandCompletedForkInPreparing(t *testing.T) {
	dir := t.TempDir()
	store := newHookStore(filepath.Join(dir, "volumes"))
	inClone := make(chan struct{})
	releaseClone := make(chan struct{})
	errDestroyFail := errors.New("destroy failed")
	var cloneOnce sync.Once
	var failDestroy atomic.Bool
	failDestroy.Store(true)
	store.onClone = func(ctx context.Context, snap, name string) error {
		if name == "ws/ws-fork-race" {
			cloneOnce.Do(func() {
				close(inClone)
				<-releaseClone
			})
		}
		return nil
	}
	store.onDestroy = func(ctx context.Context, name string) error {
		if name == "ws/ws-fork-race" && failDestroy.Load() {
			return errDestroyFail
		}
		return nil
	}

	eng := startTestEngine(t, EngineConfig{Dir: dir, Store: store})
	project := &pb.HostedProject{
		Name:  "app",
		Repos: []*pb.Repo{{Id: "app", Url: testOriginRepo(t)}},
		Steps: []*pb.Step{{Name: "build", Script: "true", EveryPull: true, Hash: "h1"}},
	}
	if _, err := eng.CreateWorkspace(t.Context(), &pb.CreateWorkspaceRequest{
		Id: "seed", Repo: "app", SessionId: "s0", Agent: "a", Project: project,
	}); err != nil {
		t.Fatal(err)
	}
	eventuallyTrue(t, "seed ready", func() bool {
		ws, err := eng.Workspace("seed")
		return err == nil && ws.State == idle
	})

	w := &workspace{rec: &pb.WorkspaceRecord{
		Workspace: &pb.Workspace{
			Id: "ws-fork-race", Project: "app", BaseId: "app-g1", NodeId: "n1", Agent: "a",
			Repos: []*pb.WorkspaceRepo{{Repo: "app"}}, State: preparing,
		},
		Volume: "ws/ws-fork-race", BaseSnapshot: "app-g1",
	}}
	eng.mu.Lock()
	eng.workspaces["ws-fork-race"] = w
	eng.saveWorkspace(w, change{what: "created"})
	eng.mu.Unlock()

	forkDone := make(chan error, 1)
	go func() {
		forkDone <- eng.fork(t.Context(), w)
	}()
	<-inClone

	relDone := make(chan error, 1)
	go func() {
		relDone <- eng.ReleaseWorkspace(t.Context(), "ws-fork-race")
	}()
	eventuallyTrue(t, "ws-fork-race releasing", func() bool {
		eng.mu.Lock()
		defer eng.mu.Unlock()
		return eng.workspaces["ws-fork-race"].releasing
	})

	// Let Store.Clone finish while ReleaseWorkspace is waiting on forkDone.
	close(releaseClone)
	if err := <-forkDone; err != nil {
		t.Fatalf("fork returned unexpected error: %v", err)
	}
	if err := <-relDone; !errors.Is(err, errDestroyFail) {
		t.Fatalf("ReleaseWorkspace err = %v, want %v", err, errDestroyFail)
	}

	ws, err := eng.Workspace("ws-fork-race")
	if err != nil {
		t.Fatal(err)
	}
	if ws.State != idle {
		t.Fatalf("ws-fork-race state after failed release = %v, want IDLE (must not strand in PREPARING)", ws.State)
	}

	// Commands queued after the failed release must run instead of hanging on PREPARING.
	cmd, err := eng.Exec(t.Context(), &pb.RunCommandRequest{
		Id: "after-failed-release", WorkspaceId: "ws-fork-race", Args: []string{"true"},
	})
	if err != nil {
		t.Fatalf("Exec after failed release: %v", err)
	}
	eventuallyTrue(t, "command finishes on recovered workspace", func() bool {
		c, ok := eng.Command(cmd.Id)
		return ok && c.State == succeeded
	})
}

// Finding #80 (board 8291): if Sync mutates the checkout on disk while
// ReleaseWorkspace sets w.releasing and then fails, workspace sync metadata
// must reflect the new checkout so a subsequent Sync back to the old tree does
// not falsely skip.
func TestFailedReleasePreservesSyncMetadata(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	origin := testOriginRepo(t)

	// Build two commits (A and B) in a client clone and bundle each against base.
	clientDir := t.TempDir()
	runGit := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command(realGit, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit(clientDir, "clone", "-q", origin, ".")
	baseCommit := runGit(clientDir, "rev-parse", "HEAD")

	makeBundle := func(name, content string) (commit, tree string, bundleBytes []byte) {
		t.Helper()
		runGit(clientDir, "checkout", "-q", "--detach", baseCommit)
		if err := os.WriteFile(filepath.Join(clientDir, "lib.txt"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(clientDir, "add", "lib.txt")
		runGit(clientDir, "commit", "-qm", name)
		commit = runGit(clientDir, "rev-parse", "HEAD")
		tree = runGit(clientDir, "rev-parse", "HEAD^{tree}")
		runGit(clientDir, "update-ref", "refs/hotbed/sync", commit)
		bundlePath := filepath.Join(t.TempDir(), name+".bundle")
		runGit(clientDir, "bundle", "create", "-q", bundlePath, baseCommit+"..refs/hotbed/sync")
		b, err := os.ReadFile(bundlePath)
		if err != nil {
			t.Fatal(err)
		}
		return commit, tree, b
	}
	commitA, treeA, bundleA := makeBundle("commit-a", "version A\n")
	commitB, treeB, bundleB := makeBundle("commit-b", "version B\n")

	dir := t.TempDir()
	store := newHookStore(filepath.Join(dir, "volumes"))
	errDestroyFail := errors.New("destroy failed during sync race")
	store.onDestroy = func(ctx context.Context, name string) error {
		if name == "ws/ws1" {
			return errDestroyFail
		}
		return nil
	}
	units := &pathPrefixUnits{Units: &fake.Units{}}
	eng := startTestEngine(t, EngineConfig{Dir: dir, Store: store, Units: units})
	project := &pb.HostedProject{
		Name:  "app",
		Repos: []*pb.Repo{{Id: "app", Url: origin}},
		Steps: []*pb.Step{{Name: "build", Script: "true", EveryPull: true, Hash: "h1"}},
	}
	if _, err := eng.CreateWorkspace(t.Context(), &pb.CreateWorkspaceRequest{
		Id: "ws1", Repo: "app", SessionId: "s1", Agent: "a", Project: project,
	}); err != nil {
		t.Fatal(err)
	}
	eventuallyTrue(t, "ws1 ready", func() bool {
		ws, err := eng.Workspace("ws1")
		return err == nil && ws.State == idle
	})

	// 1. Sync to tree A.
	respA, err := eng.Sync(t.Context(), &pb.SyncHeader{
		WorkspaceId: "ws1", Repo: "app", Commit: commitA, Tree: treeA,
	}, bytes.NewReader(bundleA))
	if err != nil {
		t.Fatalf("sync A: %v", err)
	}
	if respA.Skipped {
		t.Fatal("initial sync A unexpectedly skipped")
	}

	// 2. Wrap git so sync B pauses inside gitops.Apply (after line 510 unlocked
	// e.mu, while holding w.sync) so ReleaseWorkspace deterministically sets
	// w.releasing = true mid-apply and then fails Destroy.
	gateDir := t.TempDir()
	inApply := filepath.Join(gateDir, "in-apply")
	releaseApply := filepath.Join(gateDir, "release-apply")
	binDir := t.TempDir()
	wrapper := "#!/bin/sh\n" +
		"case \" $* \" in\n" +
		"  *\" fetch \"*|*\" unbundle \"*)\n" +
		"    if [ ! -f \"" + inApply + "\" ]; then\n" +
		"      : > \"" + inApply + "\"\n" +
		"      while [ ! -f \"" + releaseApply + "\" ]; do sleep 0.005; done\n" +
		"    fi\n" +
		"    ;;\n" +
		"esac\n" +
		"exec \"" + realGit + "\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	units.setPathPrefix(binDir)

	syncBDone := make(chan error, 1)
	go func() {
		_, err := eng.Sync(t.Context(), &pb.SyncHeader{
			WorkspaceId: "ws1", Repo: "app", Commit: commitB, Tree: treeB,
		}, bytes.NewReader(bundleB))
		syncBDone <- err
	}()
	eventuallyTrue(t, "sync B inside gitops.Apply", func() bool {
		_, err := os.Stat(inApply)
		return err == nil
	})

	relDone := make(chan error, 1)
	go func() {
		relDone <- eng.ReleaseWorkspace(t.Context(), "ws1")
	}()
	eventuallyTrue(t, "ws1 releasing while sync B applies", func() bool {
		eng.mu.Lock()
		defer eng.mu.Unlock()
		return eng.workspaces["ws1"].releasing
	})

	// Unblock gitops.Apply so disk transitions to B while w.releasing is true.
	if err := os.WriteFile(releaseApply, []byte("go"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := <-syncBDone; api.CodeOf(err) != api.CodeNotFound {
		t.Fatalf("sync B racing ReleaseWorkspace = %v, want CodeNotFound", err)
	}
	if err := <-relDone; !errors.Is(err, errDestroyFail) {
		t.Fatalf("ReleaseWorkspace err = %v, want %v", err, errDestroyFail)
	}

	// Disk is now at B, so metadata must also record tree B (not stale tree A).
	ws, err := eng.Workspace("ws1")
	if err != nil {
		t.Fatal(err)
	}
	if got := repoOf(ws, "app").Tree; got != treeB {
		t.Fatalf("workspace tree after failed release = %q, want %q", got, treeB)
	}
	libPath := filepath.Join(store.Path("ws/ws1"), "work", "app", "lib.txt")
	if got, err := os.ReadFile(libPath); err != nil || string(got) != "version B\n" {
		t.Fatalf("disk lib.txt after sync B = %q (err=%v), want %q", got, err, "version B\n")
	}

	// 3. Subsequent sync back to tree A must NOT skip based on stale metadata.
	respA2, err := eng.Sync(t.Context(), &pb.SyncHeader{
		WorkspaceId: "ws1", Repo: "app", Commit: commitA, Tree: treeA,
	}, bytes.NewReader(bundleA))
	if err != nil {
		t.Fatalf("second sync A: %v", err)
	}
	if respA2.Skipped {
		t.Fatal("sync back to tree A was mistakenly skipped due to stale metadata")
	}
	if got, err := os.ReadFile(libPath); err != nil || string(got) != "version A\n" {
		t.Fatalf("disk lib.txt after sync back to A = %q (err=%v), want %q", got, err, "version A\n")
	}
}

// Finding #81 (board 8291): if Units.Stop fails during the stop-vs-admit race,
// Stop must return the error, preserve tmpDir, and keep the command non-terminal
// so a retry of Stop calls Units.Stop again and cleans up tmpDir once stopped.
func TestStopDuringAdmitFailedUnitsStopPreservesTmpAndAllowsRetry(t *testing.T) {
	dir := t.TempDir()
	store := &fake.Store{Root: filepath.Join(dir, "volumes")}
	inStart := make(chan struct{})
	releaseStart := make(chan struct{})
	errStopFail := errors.New("systemd stop failed")
	var stopCalls atomic.Int32
	units := &raceHookUnits{
		Units: &fake.Units{},
		onStart: func(ctx context.Context, spec unit.Spec) error {
			if spec.Name == "hotbed-stop-fail-cmd" {
				close(inStart)
				<-releaseStart
			}
			return nil
		},
		onStop: func(ctx context.Context, name string) error {
			if name == "hotbed-stop-fail-cmd" {
				if stopCalls.Add(1) == 1 {
					return errStopFail
				}
			}
			return nil
		},
	}
	eng, err := NewEngine(t.Context(), EngineConfig{
		Dir: dir, NodeID: "n1", Host: "worker1",
		Store: store, BaseImage: fake.BaseImage(store), Units: units, System: fake.NewSystem(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	if err := store.Create(t.Context(), "ws/w1"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(store.Path("ws/w1"), "work", "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	eng.mu.Lock()
	eng.workspaces["w1"] = &workspace{rec: &pb.WorkspaceRecord{
		Workspace: &pb.Workspace{Id: "w1", Project: "app", State: idle, Repos: []*pb.WorkspaceRepo{{Repo: "app"}}},
		Volume:    "ws/w1",
	}}
	eng.mu.Unlock()

	if _, err := eng.Exec(t.Context(), &pb.RunCommandRequest{
		Id: "stop-fail-cmd", WorkspaceId: "w1", Args: []string{"sleep", "60"},
	}); err != nil {
		t.Fatal(err)
	}

	admitDone := make(chan struct{})
	go func() {
		defer close(admitDone)
		eng.admit(t.Context())
	}()

	<-inStart
	tmpPath := filepath.Join(store.Path("ws/w1"), tmpName, "stop-fail-cmd")
	if _, err := os.Stat(tmpPath); err != nil {
		t.Fatalf("expected tmpDir %s to exist during admit: %v", tmpPath, err)
	}

	stop1Done := make(chan error, 1)
	go func() {
		_, err := eng.Stop(t.Context(), "stop-fail-cmd", "first stop")
		stop1Done <- err
	}()
	eventuallyTrue(t, "stop-fail-cmd stopping", func() bool {
		eng.mu.Lock()
		defer eng.mu.Unlock()
		c, ok := eng.commands["stop-fail-cmd"]
		return ok && c.stopping
	})
	close(releaseStart)

	err1 := <-stop1Done
	<-admitDone

	if !errors.Is(err1, errStopFail) {
		t.Fatalf("first Stop err = %v, want %v", err1, errStopFail)
	}
	if got := stopCalls.Load(); got != 1 {
		t.Fatalf("Units.Stop calls after first Stop = %d, want 1", got)
	}
	cmd, ok := eng.Command("stop-fail-cmd")
	if !ok {
		t.Fatal("command missing after failed stop during admit")
	}
	if !api.Live(cmd.State) || cmd.State != running {
		t.Fatalf("command state after failed Units.Stop = %v, want RUNNING (non-terminal)", cmd.State)
	}
	// TMPDIR must be preserved while the unit is still running.
	if _, err := os.Stat(tmpPath); err != nil {
		t.Fatalf("tmpDir %s was removed even though Units.Stop failed: %v", tmpPath, err)
	}

	// Retry Stop: must call Units.Stop again, succeed, mark cancelled, and remove tmpDir.
	cmd2, err2 := eng.Stop(t.Context(), "stop-fail-cmd", "retry stop")
	if err2 != nil {
		t.Fatalf("retry Stop failed: %v", err2)
	}
	if cmd2.State != cancelled {
		t.Fatalf("command state after retry Stop = %v, want CANCELLED", cmd2.State)
	}
	if got := stopCalls.Load(); got != 2 {
		t.Fatalf("Units.Stop calls after retry Stop = %d, want 2", got)
	}
	if err := eng.tmpsGone(t.Context(), "w1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tmpPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tmpDir %s still exists after retry Stop succeeded: %v", tmpPath, err)
	}
}

// Finding #92 (board 8291): baseFor must clone a building base while still
// holding e.mu so concurrent freshened() mutations do not data-race.
func TestBaseForVsFreshenedRace(t *testing.T) {
	origin := testOriginRepo(t)
	project := &pb.HostedProject{
		Name:  "app",
		Repos: []*pb.Repo{{Id: "app", Url: origin}},
	}

	for iter := range 10 {
		dir := t.TempDir()
		store := &fake.Store{Root: filepath.Join(dir, "volumes")}
		eng, err := NewEngine(t.Context(), EngineConfig{
			Dir: dir, NodeID: "n1", Host: "worker1",
			Store: store, BaseImage: fake.BaseImage(store), Units: &fake.Units{}, System: fake.NewSystem(),
			AllowLocalRepos: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		h, err := eng.remoteHeads(t.Context(), project)
		if err != nil {
			eng.Close()
			t.Fatal(err)
		}
		want := recipe(project, h, eng.baseImageKey())
		vol := baseVolume("app", 1)
		if err := store.Create(t.Context(), vol); err != nil {
			eng.Close()
			t.Fatal(err)
		}
		baseID := baseSnapshot("app", 1)
		b := &pb.Base{
			Id: baseID, Project: "app", NodeId: "n1", Generation: 1,
			State: building, Cold: true, Command: "f-app-g1", Recipe: want,
			Repos: []*pb.BaseRepo{{Repo: "app", Commit: h["app"]}},
			Steps: []*pb.AppliedStep{{Name: "build"}},
		}
		c := eng.newCommand(&pb.CommandRecord{
			Command: &pb.Command{Id: "f-app-g1", Kind: "freshen", State: succeeded},
			Project: "app", BaseId: baseID, Volume: vol,
		})
		eng.mu.Lock()
		eng.bases[baseID] = b
		eng.setBaseImage(baseID, eng.baseImageKey())
		eng.commands[c.cmd().Id] = c
		eng.mu.Unlock()

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			eng.freshened(t.Context(), c, true, 1.0, 1)
		}()
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				got, _, err := eng.baseFor(t.Context(), project)
				if err != nil {
					t.Errorf("iter %d: baseFor: %v", iter, err)
					return
				}
				if got == nil || got.Id != baseID {
					t.Errorf("iter %d: baseFor got %v, want %s", iter, got, baseID)
				}
			}()
		}
		close(start)
		wg.Wait()
		eng.Close()
	}
}

// Engine.Close must wait for in-flight background work (such as forkPrepared
// spawned by freshened) before closing e.db so commits never hit
// "sql: database is closed" (board 7971).
func TestCloseWaitsForInFlightForkPrepared(t *testing.T) {
	dir := t.TempDir()
	store := newHookStore(filepath.Join(dir, "volumes"))
	eng, err := NewEngine(t.Context(), EngineConfig{
		Dir: dir, NodeID: "n1", Host: "worker1",
		Store: store, BaseImage: fake.BaseImage(store), Units: &fake.Units{}, System: fake.NewSystem(),
		AllowLocalRepos: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	vol := baseVolume("app", 1)
	if err := store.Create(t.Context(), vol); err != nil {
		eng.Close()
		t.Fatal(err)
	}
	baseID := baseSnapshot("app", 1)
	b := &pb.Base{
		Id: baseID, Project: "app", NodeId: "n1", Generation: 1,
		State: building, Cold: true, Command: "f-app-g1",
	}
	c := eng.newCommand(&pb.CommandRecord{
		Command: &pb.Command{Id: "f-app-g1", Kind: "freshen", State: succeeded},
		Project: "app", BaseId: baseID, Volume: vol,
	})
	w := &workspace{rec: &pb.WorkspaceRecord{
		Workspace:    &pb.Workspace{Id: "ws-prep", Project: "app", BaseId: baseID, State: preparing},
		Volume:       "ws/ws-prep",
		BaseSnapshot: baseID,
	}}
	eng.mu.Lock()
	eng.bases[baseID] = b
	eng.commands[c.cmd().Id] = c
	eng.workspaces[w.ws().Id] = w
	eng.mu.Unlock()

	cloneEntered := make(chan struct{})
	releaseClone := make(chan struct{})
	store.onClone = func(_ context.Context, _, _ string) error {
		close(cloneEntered)
		<-releaseClone
		return nil
	}

	// Trigger freshened -> snapshots base and spawns forkPrepared in the background.
	eng.freshened(t.Context(), c, true, 1.0, 1)

	select {
	case <-cloneEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("forkPrepared never entered Store.Clone")
	}

	// Start Close() while forkPrepared is mid-Clone: it must block waiting for
	// forkPrepared instead of closing e.db out from under it.
	closeDone := make(chan error, 1)
	go func() {
		closeDone <- eng.Close()
	}()
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned (%v) while forkPrepared was still in Store.Clone", err)
	case <-time.After(50 * time.Millisecond):
	}

	// Let Store.Clone finish (ignoring ctx, just like fake.Store's cp -a).
	// finishFork must write to e.db cleanly before Close closes it.
	close(releaseClone)
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after forkPrepared finished")
	}

	eng.mu.Lock()
	dirty := eng.dirtyGen
	state := w.ws().State
	eng.mu.Unlock()
	if dirty != 0 {
		t.Errorf("dirtyGen = %d, want 0 (e.db closed before forkPrepared saved workspace)", dirty)
	}
	if state != idle {
		t.Errorf("workspace state = %v, want IDLE", state)
	}
}
