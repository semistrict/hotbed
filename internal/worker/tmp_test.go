package worker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/statedir"
)

// A cladefs FUSE workspace's files are at its mount, not its store volume:
// the sweep finds the temporary directories of dead commands there.
func TestSweepTmpFindsCladefsFUSEWorkspaceMount(t *testing.T) {
	mount := t.TempDir()
	dead := filepath.Join(mount, tmpName, "dead-cmd")
	liveDir := filepath.Join(mount, tmpName, "live-cmd")
	for _, d := range []string{dead, liveDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	live := &command{rec: &pb.CommandRecord{Command: &pb.Command{Id: "live-cmd", State: running}}}
	fuse := newCladefsFUSEBackend("", statedir.CladefsFUSEConfig{})
	fuse.wsMounts["ws-f"] = fuseWSMount{MountPath: mount}
	e := &Engine{
		cfg: EngineConfig{
			Store:       &fake.Store{Root: t.TempDir()},
			CladefsFUSE: fuse,
		},
		commands: map[string]*command{"live-cmd": live},
		workspaces: map[string]*workspace{"ws-f": {rec: &pb.WorkspaceRecord{
			Workspace: &pb.Workspace{Id: "ws-f", State: idle},
			Volume:    "ws/ws-f",
		}}},
	}
	e.sweepTmp()
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Fatalf("a dead command's tmp at the FUSE mount stays (%v)", err)
	}
	if _, err := os.Stat(liveDir); err != nil {
		t.Fatalf("a live command's tmp went: %v", err)
	}
}
