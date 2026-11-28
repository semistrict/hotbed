package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/cmdlog"
	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/settle"
)

// outputOf is the output of a command that wrote log and ended.
func outputOf(t *testing.T, log string) *output {
	t.Helper()
	path := filepath.Join(t.TempDir(), "c.log")
	if err := os.WriteFile(path, []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
	o := newOutput(path)
	o.finish()
	return o
}

func TestOutputSortsFailures(t *testing.T) {
	one := int32(1)
	for _, tc := range []struct {
		name, log, kind string
		state           pb.CommandState
		code            *int32
		units           int
		failure         string
	}{
		{"succeeded", "   Compiling a v1\n   Compiling b v2\n", "build", succeeded, new(int32), 2, ""},
		{"signal", "   Compiling a v1\n", "build", failed, nil, 1, "signal"},
		{"compile", "   Compiling a v1\nerror[E0308]: mismatched types\n", "build", failed, &one, 1, "compile"},
		{"lint", "error: unused import\n", "check", failed, &one, 0, "lint"},
		{"test", "error: oops\ntest result: FAILED. 1 passed; 1 failed\n", "test", failed, &one, 0, "test"},
		{"go test", "--- FAIL: TestX (0.00s)\n", "test", failed, &one, 0, "test"},
		{"go package", "FAIL\tgithub.com/x/y\t0.1s\n", "test", failed, &one, 0, "test"},
		{"nextest", "        FAIL [   0.004s] crate tests::it\n", "test", failed, &one, 0, "test"},
		{"jest", "Tests:       2 failed, 5 passed, 7 total\n", "test", failed, &one, 0, "test"},
		{"not an error", "  no error: here\n", "build", failed, &one, 0, "exit"},
		{"exit", "nothing to see\n", "shell", failed, &one, 0, "exit"},
		// A last line without a newline counts once the command ends.
		{"last line", "ok\nerror: no newline", "build", failed, &one, 0, "compile"},
		{"crlf", "   Compiling a v1\r\nerror: x\r\n", "build", failed, &one, 1, "compile"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			units, failure := outputOf(t, tc.log).result(tc.state, tc.code, tc.kind)
			if units != tc.units || failure != tc.failure {
				t.Errorf("units %d failure %q, want %d %q", units, failure, tc.units, tc.failure)
			}
		})
	}
}

// Output catches up from where it left off as the log grows, and a line
// split across catch-ups counts once, whole.
func TestOutputReadsAGrowingLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	o := newOutput(path)
	o.catchUp() // no output yet
	for _, chunk := range []string{"   Compiling a v1\n   Comp", "iling b v1\n", "   Compiling c v1"} {
		if _, err := f.WriteString(chunk); err != nil {
			t.Fatal(err)
		}
		o.catchUp()
	}
	if units, _ := o.result(succeeded, new(int32), "build"); units != 2 {
		t.Errorf("units while running = %d, want 2: the third line is not finished", units)
	}
	o.finish()
	if units, _ := o.result(succeeded, new(int32), "build"); units != 3 {
		t.Errorf("units once ended = %d, want 3", units)
	}
}

// A line far longer than any scan buffer neither stops the scan nor hides
// what follows it.
func TestOutputSurvivesAHugeLine(t *testing.T) {
	log := "   Compiling a v1\n" + strings.Repeat("x", 8<<20) + "\nerror: after the long line\n   Compiling b v1\n"
	units, failure := outputOf(t, log).result(failed, new(int32), "build")
	if units != 2 || failure != "compile" {
		t.Errorf("units %d failure %q, want 2 compile", units, failure)
	}
}

// outputEngine is an engine on fakes with workspace w, keeping logs to
// limit (zero for the default).
func outputEngine(t *testing.T, limit cmdlog.Limit) (*Engine, *fake.Units) {
	t.Helper()
	units, store := &fake.Units{}, &fake.Store{Root: t.TempDir()}
	e, err := NewEngine(t.Context(), EngineConfig{
		Dir: t.TempDir(), NodeID: "n", Host: "asahimbp", Store: store, BaseImage: fake.BaseImage(store), Units: units, System: fake.NewSystem(),
		LogLimit: limit,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	if err := os.MkdirAll(filepath.Join(store.Path("v"), "work", "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.workspaces["w"] = &workspace{rec: &pb.WorkspaceRecord{Workspace: &pb.Workspace{Id: "w", Project: "app", Repos: []*pb.WorkspaceRepo{{Repo: "app"}}, State: idle}, Volume: "v"}}
	return e, units
}

// The engine sorts a failed command by what its output says, its last line
// too though it lacks a newline, read as the command ran and after its unit
// is gone, not from the whole log under the engine's lock.
func TestEndedCommandIsSortedByItsOutput(t *testing.T) {
	e, units := outputEngine(t, cmdlog.Limit{})
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
		Id: "c-1", WorkspaceId: "w", Kind: "build",
		Args: []string{"sh", "-c", `echo "   Compiling a v1"; printf "error: broken"; exit 1`},
	}); err != nil {
		t.Fatal(err)
	}
	e.admit(t.Context())
	if err := units.Wait(t.Context(), "", "hotbed-c-1"); err != nil {
		t.Fatal(err)
	}
	e.step(t.Context())
	if s := e.state("c-1"); s != failed {
		t.Fatalf("c-1 is %v, want failed", s)
	}
	evs, err := e.db.events(0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var finished *pb.FinishedCommand
	for _, ev := range evs {
		if f := ev.GetCommand().GetFinished(); f != nil {
			finished = f
		}
	}
	if finished.GetFailure() != "compile" {
		t.Errorf("finished %v, want failure compile", finished)
	}
}

// A running command's log past the engine's limit is compacted as it runs:
// its disk use stays near the limit, the command says how much it dropped,
// and its end records it.
func TestRunningCommandsLogKeepsToTheLimit(t *testing.T) {
	e, units := outputEngine(t, cmdlog.Limit{Head: 1 << 20, Tail: 2 << 20})
	// 20 MiB of output, then it waits to be stopped.
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
		Id: "c-1", WorkspaceId: "w",
		Args: []string{"sh", "-c", "head -c 20971520 /dev/zero | tr '\\0' x; echo; echo done; sleep 300"},
	}); err != nil {
		t.Fatal(err)
	}
	e.admit(t.Context())
	t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-c-1") })
	path, err := e.logPath("c-1")
	if err != nil {
		t.Fatal(err)
	}
	eventuallyTrue(t, "the command to write it all", func() bool {
		lines, _, _ := cmdlog.Tail(path, 1, 100)
		return len(lines) == 1 && lines[0] == "done"
	})
	e.step(t.Context())
	e.mu.Lock()
	dropped := e.commands["c-1"].cmd().DroppedOutputBytes
	e.mu.Unlock()
	if dropped != 17<<20 {
		t.Errorf("dropped %d bytes, want 17 MiB: 20 MiB less a 1 MiB head and a 2 MiB tail", dropped)
	}
	disk, err := cmdlog.DataBytes(path)
	if err != nil {
		t.Fatal(err)
	}
	if disk > 4<<20 {
		t.Errorf("the log holds %d non-hole bytes, want about 3 MiB", disk)
	}
	ended, err := e.Stop(t.Context(), "c-1", "test")
	if err != nil {
		t.Fatal(err)
	}
	if ended.DroppedOutputBytes != 17<<20 {
		t.Errorf("ended with %d bytes dropped, want 17 MiB", ended.DroppedOutputBytes)
	}
}

// eventuallyTrue waits up to 30s for cond.
func eventuallyTrue(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A command that writes a lot just before it exits has its log compacted
// before the rest is read, so the read is at most the limit, and its end
// records what was dropped.
func TestEndedCommandsLogIsCompactedBeforeItIsRead(t *testing.T) {
	e, units := outputEngine(t, cmdlog.Limit{Head: 1 << 20, Tail: 2 << 20})
	if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
		Id: "c-1", WorkspaceId: "w",
		Args: []string{"sh", "-c", "head -c 20971520 /dev/zero | tr '\\0' x"},
	}); err != nil {
		t.Fatal(err)
	}
	e.admit(t.Context())
	if err := units.Wait(t.Context(), "", "hotbed-c-1"); err != nil {
		t.Fatal(err)
	}
	e.step(t.Context())
	e.mu.Lock()
	cmd := clone(e.commands["c-1"].cmd())
	e.mu.Unlock()
	if cmd.State != succeeded || cmd.DroppedOutputBytes != 17<<20 {
		t.Errorf("ended %v with %d bytes dropped, want succeeded with 17 MiB", cmd.State, cmd.DroppedOutputBytes)
	}
	if off := e.commands["c-1"].out.lines.Offset(); off != 20<<20 {
		t.Errorf("read to %d, want the log's end", off)
	}
}

func TestNewEngineRefusesALogLimitWithNoTail(t *testing.T) {
	store := &fake.Store{Root: t.TempDir()}
	_, err := NewEngine(t.Context(), EngineConfig{
		Dir: t.TempDir(), NodeID: "n", Host: "asahimbp", Store: store, BaseImage: fake.BaseImage(store), Units: &fake.Units{}, System: fake.NewSystem(),
		LogLimit: cmdlog.Limit{Head: 1 << 20},
	})
	if err == nil || err.Error() != "log limit {Head:1048576 Tail:0}: keep a head of at least 0 bytes and a tail of at least 1048576" {
		t.Errorf("NewEngine with no tail: %v", err)
	}
}

// A TTY command records its PTY column width in a .tty sidecar so QueryOutput
// still wraps space-padded progress rows even after the command is forgotten
// from live engine memory, and cmdlog.Remove cleans the sidecar up.
func TestQueryOutputUsesTTYSidecarAfterCommandForgotten(t *testing.T) {
	e, _ := outputEngine(t, cmdlog.Limit{})
	path := filepath.Join(e.logsDir(), "c-tty.log")
	pad := func(s string) string {
		return s + strings.Repeat(" ", settle.ScreenCols-len(s))
	}
	raw := pad("Running [00:01] 0/1") + "\x1b[1A\r\x1b[2K        FAIL [0.05s] pkg::broken\r\n"
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmdlog.WriteTTYCols(path, settle.ScreenCols); err != nil {
		t.Fatal(err)
	}

	q, err := e.QueryOutput(t.Context(), &pb.QueryOutputRequest{Id: "c-tty", Grep: "FAIL"})
	if err != nil {
		t.Fatalf("QueryOutput: %v", err)
	}
	if q.GetMatches() != 1 || len(q.Lines) != 1 || !strings.Contains(q.Lines[0].Text, "FAIL [0.05s] pkg::broken") {
		t.Fatalf("QueryOutput on forgotten TTY log = %+v, want 1 FAIL match", q)
	}

	if err := cmdlog.Remove(path); err != nil {
		t.Fatalf("cmdlog.Remove: %v", err)
	}
	if cols := cmdlog.ReadTTYCols(path); cols != 0 {
		t.Fatalf("ReadTTYCols after Remove = %d, want 0", cols)
	}
}
