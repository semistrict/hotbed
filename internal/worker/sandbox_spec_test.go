package worker

import (
	"os"
	"slices"
	"testing"

	"github.com/semistrict/hotbed/internal/baseimage"
	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/unit"
)

// Every command runs chrooted inside its workspace volume with CAP_PERFMON
// and tracefs so builds, tests, and measurements all have perf, cgroups, and
// passwordless sudo to root.
func TestCommandsRunChrootedWithPerfmonAndTraceFS(t *testing.T) {
	store := &fake.Store{Root: t.TempDir()}
	e, err := NewEngine(t.Context(), EngineConfig{
		Dir: t.TempDir(), NodeID: "n", Host: "ubu1", Bench: fake.NewIsolation("7-10", 7),
		Store: store, Units: &fake.Units{}, System: fake.NewSystem(),
		BaseImage: &baseimage.Manager{Store: store},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	if err := os.MkdirAll(store.Path("v"), 0o755); err != nil {
		t.Fatal(err)
	}
	spec := func(bench bool) unit.Spec {
		e.mu.Lock()
		defer e.mu.Unlock()
		s, err := e.spec(&command{rec: &pb.CommandRecord{Volume: "v", Dir: "/work", Command: &pb.Command{Id: "c-1", Args: []string{"true"}, Bench: bench, Limits: &pb.CommandLimits{}}}})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	for _, bench := range []bool{false, true} {
		s := spec(bench)
		if s.RootDir != store.Path("v") || !s.TraceFS || !slices.Equal(s.Capabilities, []string{"CAP_PERFMON"}) {
			t.Errorf("bench=%v spec: root=%q tracefs=%v caps=%v; want chroot with CAP_PERFMON and TraceFS", bench, s.RootDir, s.TraceFS, s.Capabilities)
		}
		if s.NoSwap != bench {
			t.Errorf("bench=%v spec.NoSwap = %v, want %v", bench, s.NoSwap, bench)
		}
	}
}

// The worker's git credential helpers are bound read-only into a rootfs:
// the worker runs the same helpers on the host, and root of a sandbox may
// write a read-write idmapped bind of root's files.
func TestGitAuthIsReadOnlyInARootfs(t *testing.T) {
	e := &Engine{cfg: EngineConfig{BaseImage: &baseimage.Manager{}}, gitAuth: &gitAuthServer{dir: "/var/lib/hotbed/git-auth", sshProxy: &gitSSHProxy{}}}
	var spec unit.Spec
	e.inRootfs(&spec, "/vol/ws/a")
	bind := unit.Bind{Source: "/var/lib/hotbed/git-auth", Target: gitAuthChrootDir}
	if !slices.Contains(spec.ReadOnlyBinds, bind) || slices.Contains(spec.Binds, bind) {
		t.Errorf("binds %v, read-only binds %v; want git-auth read-only only", spec.Binds, spec.ReadOnlyBinds)
	}
}

// NewEngine fails hard when BaseImage is nil rather than silently falling back
// to host-root execution (board 7872).
func TestNewEngineRequiresBaseImage(t *testing.T) {
	store := &fake.Store{Root: t.TempDir()}
	if _, err := NewEngine(t.Context(), EngineConfig{
		Dir: t.TempDir(), NodeID: "n", Host: "ubu1",
		Store: store, Units: &fake.Units{}, System: fake.NewSystem(),
	}); err == nil {
		t.Fatal("NewEngine accepted nil BaseImage, want error")
	}
}
