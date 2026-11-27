package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/setup"
)

// A command on a workspace of this machine's own worker goes to that worker's
// Unix socket, whichever command it is: sync once looked the worker up by
// the workspace's address instead of its node, and went through the
// controller. The socket here answers 503 to everything, so a call that
// reaches it fails, and one that does not would succeed.
func TestSyncReachesThisMachinesWorkerOnItsSocket(t *testing.T) {
	c := newCluster(t)
	origin := upstream(t)
	c.addRepo(origin)
	var sess pb.Session
	c.ok(&sess, "session", "start")
	ws := c.workspace(sess.Id, nil)

	// A Unix socket's path is at most about 100 bytes: t.TempDir's is too
	// long on macOS.
	sockDir, err := os.MkdirTemp("/tmp", "hb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "w.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "this is the local worker's socket", http.StatusServiceUnavailable)
	}))
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	c.cli.localWorker = func() (setup.LocalWorkerSocket, error) {
		return setup.LocalWorkerSocket{NodeID: ws.NodeId, Socket: sock}, nil
	}

	checkout := filepath.Join(t.TempDir(), "agent")
	gitIn(t, filepath.Dir(checkout), "clone", "--quiet", origin, checkout)
	t.Chdir(checkout)
	if code, out, stderr := c.hotbed("sync", "-s", sess.Id, "-r", "app"); code == 0 {
		t.Errorf("sync with the local worker's socket answering 503 succeeded: stdout %q, stderr %q", out, stderr)
	}
	if hits.Load() == 0 {
		t.Error("sync never called this machine's worker on its socket")
	}
}
