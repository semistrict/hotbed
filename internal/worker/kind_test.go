package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// Commands without an explicit kind default to "server" only when Port > 0,
// and "shell" otherwise — argv substrings such as " run " or "nextest" never
// silently reclassify a command as a server or build (board 7876).
func TestCommandKindDefaultsWithoutArgvInference(t *testing.T) {
	e, _, _, _ := parkEngine(t)

	for _, tc := range []struct {
		id   string
		args []string
		port int32
		kind string
		want string
	}{
		{"nextest", []string{"cargo", "nextest", "run", "--workspace"}, 0, "", "shell"},
		{"cargo-run", []string{"cargo", "run", "--bin", "app"}, 0, "", "shell"},
		{"go-run", []string{"go", "run", "./cmd/foo"}, 0, "", "shell"},
		{"pnpm-dev", []string{"pnpm", "dev", "--host"}, 0, "", "shell"},
		{"port-server", []string{"pnpm", "dev"}, 5173, "", "server"},
		{"port-shell", []string{"sh", "-c", "sleep 60"}, 8080, "", "server"},
		{"explicit-server", []string{"sh", "-c", "sleep 60"}, 0, "server", "server"},
		{"explicit-build", []string{"cargo", "build"}, 0, "build", "build"},
		{"explicit-test", []string{"cargo", "nextest", "run", "--workspace"}, 0, "test", "test"},
		{"explicit-check", []string{"cargo", "clippy"}, 0, "check", "check"},
		{"explicit-freshen", []string{"true"}, 0, "freshen", "freshen"},
		{"explicit-shell", []string{"cargo", "build"}, 0, "shell", "shell"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			cmd, err := e.Exec(t.Context(), &pb.RunCommandRequest{
				Id:          tc.id,
				WorkspaceId: "a",
				Args:        tc.args,
				Port:        tc.port,
				Kind:        tc.kind,
			})
			if err != nil {
				t.Fatalf("Exec(%s): %v", tc.id, err)
			}
			if cmd.Kind != tc.want {
				t.Errorf("Exec(%v, port=%d, kind=%q).Kind = %q, want %q", tc.args, tc.port, tc.kind, cmd.Kind, tc.want)
			}
		})
	}
}

// Commands like `cargo nextest run --workspace` or `go run ./cmd/foo` without
// Port > 0 or Kind: "server" are not treated as servers: they can be parked
// under memory pressure and count toward bench CPU accounting, while Port > 0
// and explicit Kind: "server" commands remain excluded from both (board 7876).
func TestNonServerRunCommandsCountInBenchAndKeepRunningWhenMemoryIsFull(t *testing.T) {
	units, sys := &fake.Units{}, fake.NewSystem()
	store := &fake.Store{Root: t.TempDir()}
	now := time.Unix(1795400000, 0)
	e, err := NewEngine(t.Context(), EngineConfig{
		Dir: t.TempDir(), NodeID: "n", Host: "asahimbp",
		Store: store, BaseImage: fake.BaseImage(store), Units: units, System: sys,
		Bench: fake.NewIsolation("4-7", 4),
		Now:   func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	for _, w := range []string{"a", "b", "c"} {
		if err := os.MkdirAll(filepath.Join(store.Path("ws/"+w), "work", "app"), 0o755); err != nil {
			t.Fatal(err)
		}
		e.workspaces[w] = &workspace{rec: &pb.WorkspaceRecord{
			Workspace: &pb.Workspace{Id: w, Project: "app", Repos: []*pb.WorkspaceRepo{{Repo: "app"}}, State: idle},
			Volume:    "ws/" + w,
		}}
	}

	launch := func(id, ws string, args []string, port int32, kind string) *command {
		t.Helper()
		now = now.Add(time.Second)
		if _, err := e.Exec(t.Context(), &pb.RunCommandRequest{
			Id: id, WorkspaceId: ws, Args: args, Port: port, Kind: kind,
		}); err != nil {
			t.Fatalf("Exec %s: %v", id, err)
		}
		t.Cleanup(func() { units.Stop(context.WithoutCancel(t.Context()), "hotbed-"+id) })
		e.step(t.Context())
		c := e.commands[id]
		if c.cmd().State != running {
			t.Fatalf("%s is %v: %s", id, c.cmd().State, c.cmd().Reason)
		}
		return c
	}

	// Pass the offending substrings ("cargo nextest run --workspace",
	// "go run ./cmd/foo") as shell arguments so real processes run under fake.Units.
	nextest := launch("nextest-1", "a", []string{"sh", "-c", "sleep 60", "--", "cargo", "nextest", "run", "--workspace"}, 0, "")
	goRun := launch("gorun-1", "b", []string{"sh", "-c", "sleep 60", "--", "go", "run", "./cmd/foo"}, 0, "")
	portSrv := launch("port-srv", "c", []string{"sh", "-c", "sleep 60"}, 5173, "")
	explicitSrv := launch("kind-srv", "c", []string{"sh", "-c", "sleep 60"}, 0, "server")

	e.mu.Lock()
	nextest.cmd().CpuCores = 2.5
	goRun.cmd().CpuCores = 1.5
	portSrv.cmd().CpuCores = 3.0
	explicitSrv.cmd().CpuCores = 4.0
	e.mu.Unlock()

	if nextest.cmd().Kind != "shell" || goRun.cmd().Kind != "shell" {
		t.Fatalf("kinds = nextest:%q goRun:%q, want both shell", nextest.cmd().Kind, goRun.cmd().Kind)
	}
	if portSrv.cmd().Kind != "server" || explicitSrv.cmd().Kind != "server" {
		t.Fatalf("server kinds = portSrv:%q explicitSrv:%q, want both server", portSrv.cmd().Kind, explicitSrv.cmd().Kind)
	}

	// Bench CPU accounting counts nextest (2.5) + goRun (1.5) = 4.0, excluding both servers.
	e.mu.Lock()
	gotCores := e.workCores()
	e.mu.Unlock()
	if gotCores != 4.0 {
		t.Errorf("workCores = %v, want 4.0 (nextest + goRun only, excluding servers)", gotCores)
	}

	// With memory full, nothing already running is frozen: it swaps.
	sys.SetUsed(sys.Mem.Total * 95 / 100)
	sys.Psi = 60
	e.step(t.Context())
	for _, c := range []*command{nextest, goRun, portSrv, explicitSrv} {
		if s := c.cmd().State; s != running {
			t.Errorf("%s state = %v at 95%% memory and 60%% pressure, want running", c.cmd().Id, s)
		}
	}
}
