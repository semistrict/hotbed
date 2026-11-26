package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/httpx"
	"github.com/semistrict/hotbed/internal/setup"
)

// Clients call workspaces only through the controller, which relays each
// call to the workspace's worker over the worker's own tunnel to it, or, for
// this machine's own worker, over the worker's Unix socket: never a worker
// directly over the network.

// workerClients is the clients that reach one workspace's worker: calls for
// unary calls, stream for streams, which have no deadline; and the
// controller, whose history keeps the commands the worker has forgotten.
type workerClients struct {
	calls, stream hotbedv1connect.WorkspaceServiceClient
	controller    hotbedv1connect.WorkspaceServiceClient
}

// reach is the clients that reach node's worker: through the controller,
// or over the Unix socket of this machine's worker when it is node's.
func (f controllerClient) reach(d deps, node string) (workerClients, error) {
	calls, err := f.clients(d.http)
	if err != nil {
		return workerClients{}, err
	}
	stream, err := f.clients(d.stream)
	if err != nil {
		return workerClients{}, err
	}
	w := workerClients{calls: calls.workspaces, stream: stream.workspaces, controller: calls.workspaces}
	local, err := setup.LocalWorker("")
	if err != nil || local.Socket == "" || node == "" || local.NodeID != node {
		return w, nil
	}
	interceptors := rideOut(d, "this machine's worker")
	uc, err := api.Dial("http://worker", api.WorkerPort, unixHTTP(local.Socket, d.http.Timeout), interceptors...)
	if err != nil {
		return workerClients{}, err
	}
	us, err := api.Dial("http://worker", api.WorkerPort, unixHTTP(local.Socket, 0), interceptors...)
	if err != nil {
		return workerClients{}, err
	}
	w.calls, w.stream = hotbedv1connect.NewWorkspaceServiceClient(uc), hotbedv1connect.NewWorkspaceServiceClient(us)
	return w, nil
}

// unixHTTP is an HTTP client whose every connection is to the Unix socket
// at sock, with timeout (0 for none, for streams).
func unixHTTP(sock string, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
			Protocols: httpx.Protocols(),
		},
	}
}

// diagnoseCallTimeout bounds a direct StopCommand/PauseCommand RPC when
// --diagnose is set (sample + inspect on the worker can take up to a minute).
const diagnoseCallTimeout = 2 * time.Minute

// interventionClient returns the worker client and context for a stop or pause
// call: when diagnose is set, it uses w.stream (no 60s HTTP timeout) bounded
// by diagnoseCallTimeout.
func (w workerClients) interventionClient(ctx context.Context, diagnose bool) (context.Context, context.CancelFunc, hotbedv1connect.WorkspaceServiceClient) {
	if !diagnose {
		return ctx, func() {}, w.calls
	}
	dctx, cancel := context.WithTimeout(ctx, diagnoseCallTimeout)
	return dctx, cancel, w.stream
}

// command is a command as its worker has it or, once the worker has
// forgotten it (a while after it ended), as the controller's history keeps
// it.
func (w workerClients) command(ctx context.Context, id string) (*pb.Command, error) {
	c, err := w.calls.GetCommand(ctx, &pb.GetCommandRequest{Id: id})
	if err != nil && api.CodeOf(err) == api.CodeNotFound {
		return w.controller.GetCommand(ctx, &pb.GetCommandRequest{Id: id})
	}
	return c, err
}

// workspaceFor asks the controller where a run of req's session goes, with
// the workspace the session has there (or gets), or a new throwaway one
// (req.Throwaway). req.Repo is the project's repo the run syncs and runs in,
// "" for none; req.SyncDir is its checkout, which the run syncs first, ""
// for none.
func (f controllerClient) workspaceFor(ctx context.Context, d deps, req *pb.CreateWorkspaceRequest) (*pb.Workspace, workerClients, error) {
	// CreateWorkspace can legitimately take longer than d.http's 60s timeout
	// while preparing a base or under heavy worker load (board 7957).
	cl, err := f.clients(d.stream)
	if err != nil {
		return nil, workerClients{}, err
	}
	ws, err := createWaking(ctx, d, cl.workspaces, req)
	if err != nil {
		return nil, workerClients{}, err
	}
	if req.Throwaway && !ws.Throwaway {
		// A controller or worker older than --fork: what it gave may be
		// the session's own workspace, not to be run in as a throwaway.
		return nil, workerClients{}, api.Errorf(api.CodePlatformNotSupported, "%s gave workspace %s, which is not throwaway: it predates hotbed run --fork", ws.Node, ws.Id).
			WithSuggestion("run without --fork, or once the controller and workers run this build (hotbed nodes)")
	}
	w, err := f.reach(d, ws.NodeId)
	if err != nil {
		return nil, workerClients{}, err
	}
	if !ws.Throwaway {
		remember(d, req.SessionId, ws)
	}
	return ws, w, nil
}

// createWorkspaceTimeout bounds CreateWorkspace on d.stream so a live-but-stuck
// controller cannot hang `hotbed run` forever, while still allowing >60s base
// prep and wakeRideOut (5m).
var createWorkspaceTimeout = 10 * time.Minute

// wakeRideOut is how long a run waits for the node it goes to to wake.
const wakeRideOut = 5 * time.Minute

// createWaking creates the workspace, calling again while the node placement
// picked is waking up (NODE_WAKING, up to wakeRideOut) or while all eligible
// nodes are full and the run is queued at the controller (NODES_BUSY, until
// any eligible node frees up), saying so once on stderr. Each individual RPC
// is bounded by createWorkspaceTimeout so a stuck controller fails promptly.
func createWaking(ctx context.Context, d deps, ws hotbedv1connect.WorkspaceServiceClient, req *pb.CreateWorkspaceRequest) (*pb.Workspace, error) {
	wakeDeadline := time.Now().Add(wakeRideOut)
	saidWake, saidBusy := false, false
	for {
		call := ctx
		var cancel context.CancelFunc
		if createWorkspaceTimeout > 0 {
			call, cancel = context.WithTimeout(ctx, createWorkspaceTimeout)
		}
		w, err := ws.CreateWorkspace(call, req)
		if cancel != nil {
			cancel()
		}
		switch api.CodeOf(err) {
		case api.CodeNodeWaking:
			if time.Now().After(wakeDeadline) || ctx.Err() != nil {
				return w, err
			}
			if !saidWake {
				fmt.Fprintf(d.stderr, "hotbed: %s; waiting up to %s for it\n", api.FromConnect(err).Message, wakeRideOut)
				saidWake = true
			}
		case api.CodeNodesBusy:
			if ctx.Err() != nil {
				return w, err
			}
			if !saidBusy {
				fmt.Fprintf(d.stderr, "hotbed: %s\n", api.FromConnect(err).Message)
				saidBusy = true
			}
		default:
			return w, err
		}
	}
}

// onWorker is the workspace found, as its worker has it now.
func (f controllerClient) onWorker(ctx context.Context, d deps, found *pb.Workspace) (*pb.Workspace, workerClients, error) {
	w, err := f.reach(d, found.NodeId)
	if err != nil {
		return nil, workerClients{}, err
	}
	ws, err := w.calls.GetWorkspace(ctx, &pb.GetWorkspaceRequest{Id: found.Id})
	if err != nil {
		return nil, workerClients{}, err
	}
	ws.NodeAddr, ws.Node = found.NodeAddr, found.Node
	if ws.Project == "" {
		ws.Project = found.Project
	}
	return ws, w, nil
}

// locate finds the session's workspace (on node, when it names one) and its
// worker, and returns the workspace as its worker has it now. A session
// with workspaces on several nodes needs node to say which.
func (f controllerClient) locate(ctx context.Context, d deps, session, node string) (*pb.Workspace, workerClients, error) {
	cl, err := f.clients(d.http)
	if err != nil {
		return nil, workerClients{}, err
	}
	list, err := cl.workspaces.ListWorkspaces(ctx, &pb.ListWorkspacesRequest{SessionId: session})
	if err != nil {
		return nil, workerClients{}, err
	}
	var found []*pb.Workspace
	for _, w := range ownWorkspaces(list.Workspaces) {
		if node == "" || w.Node == node {
			found = append(found, w)
		}
	}
	switch len(found) {
	case 0:
		where := ""
		if node != "" {
			where = " on " + node
		}
		return nil, workerClients{}, api.Errorf(api.CodeNotFound, "session %s has no workspace%s", session, where).
			WithSuggestion(fmt.Sprintf("hotbed run -s %s -- CMD gives it one (hotbed workspace ls lists those it has)", session))
	case 1:
	default:
		var nodes []string
		for _, w := range found {
			nodes = append(nodes, w.Node)
		}
		return nil, workerClients{}, api.Errorf(api.CodeConflict, "session %s has workspaces on %s", session, strings.Join(nodes, ", ")).
			WithSuggestion("pass --node HOST to say which")
	}
	ws, w, err := f.onWorker(ctx, d, found[0])
	if err != nil {
		return nil, workerClients{}, err
	}
	remember(d, session, ws)
	return ws, w, nil
}

// checkoutOf is the root of the checkout this is run in when it is one of
// repo, which a run syncs; "" when it is not. explicit is the repo the
// caller named, if it named one: otherwise repo was inferred from this very
// checkout.
func (f controllerClient) checkoutOf(ctx context.Context, d deps, session, explicit, repo string) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	root, err := gitCheckoutRoot(ctx, cwd)
	if err != nil {
		return "", nil // not in a checkout
	}
	if explicit == "" {
		return root, nil
	}
	here, err := f.resolveRepo(ctx, d, session, "", root)
	if err != nil {
		if api.CodeOf(err) == api.CodeInvalidArgs {
			return "", nil // a checkout of no repo the cluster has
		}
		return "", err
	}
	if here != repo {
		return "", nil
	}
	return root, nil
}

// findCommand finds the command its ID names, whichever session's (or a
// base's freshen, which is no session's), and its worker.
func (f controllerClient) findCommand(ctx context.Context, d deps, _, id string) (*pb.Command, workerClients, error) {
	cl, err := f.clients(d.http)
	if err != nil {
		return nil, workerClients{}, err
	}
	cmd, err := cl.workspaces.GetCommand(ctx, &pb.GetCommandRequest{Id: id})
	if err != nil {
		return nil, workerClients{}, err
	}
	w, err := f.reach(d, cmd.NodeId)
	return cmd, w, err
}

// findOwnCommand is findCommand for a call that changes the command: it
// must be the session's own, so an agent acts on another's run only by
// naming that session.
func (f controllerClient) findOwnCommand(ctx context.Context, d deps, session, id string) (*pb.Command, workerClients, error) {
	cmd, w, err := f.findCommand(ctx, d, session, id)
	if err != nil {
		return nil, workerClients{}, err
	}
	if cmd.SessionId != session {
		return nil, workerClients{}, api.Errorf(api.CodeNotFound, "session %s has no command %s", session, id).
			WithSuggestion(ownerSuggestion(cmd))
	}
	return cmd, w, nil
}

// ownerSuggestion says how to act on a command of another session.
func ownerSuggestion(cmd *pb.Command) string {
	if cmd.SessionId == "" {
		return fmt.Sprintf("%s is a base's freshen, which belongs to no session", cmd.Id)
	}
	return fmt.Sprintf("%s is session %s's: pass -s %s to act on it", cmd.Id, cmd.SessionId, cmd.SessionId)
}

// known is where this machine last used a session's workspaces, each by
// ID, and the one it used last: the checkouts it synced into them, which a
// run infers its repo from.
type known struct {
	Last       string          `json:"last,omitempty"`
	Workspaces map[string]spot `json:"workspaces,omitempty"`
}

type spot struct {
	Node    string `json:"node,omitempty"`
	Project string `json:"project,omitempty"`
	// The checkouts synced into it, by repo.
	SyncDirs map[string]string `json:"syncDirs,omitempty"`
}

func knownFile(d deps, session string) string {
	return filepath.Join(d.cache, "sessions", session+".workspaces.json")
}

func loadKnown(d deps, session string) (known, error) {
	b, err := os.ReadFile(knownFile(d, session))
	if err != nil {
		return known{}, err
	}
	var k known
	return k, json.Unmarshal(b, &k)
}

// remember notes a session's workspace and the checkouts synced into it.
func remember(d deps, session string, ws *pb.Workspace) {
	k, err := loadKnown(d, session)
	if err != nil {
		k = known{}
	}
	sp := spot{Node: ws.Node, Project: ws.Project}
	for _, r := range ws.Repos {
		if r.SyncDir != "" {
			if sp.SyncDirs == nil {
				sp.SyncDirs = map[string]string{}
			}
			sp.SyncDirs[r.Repo] = r.SyncDir
		}
	}
	if k.Last == ws.Id && reflect.DeepEqual(k.Workspaces[ws.Id], sp) {
		return
	}
	if k.Workspaces == nil {
		k.Workspaces = map[string]spot{}
	}
	k.Last, k.Workspaces[ws.Id] = ws.Id, sp
	if err := saveKnown(d, session, k); err != nil {
		fmt.Fprintf(d.stderr, "hotbed: could not note where %s is: %v\n", ws.Id, err)
	}
}

// ownWorkspaces are the session's own workspaces among ws: all but its
// throwaway ones, which are each one command's.
func ownWorkspaces(ws []*pb.Workspace) []*pb.Workspace {
	return slices.DeleteFunc(slices.Clone(ws), func(w *pb.Workspace) bool { return w.Throwaway })
}

// forget drops a session's released workspace from what this machine
// knows.
func forget(d deps, session, id string) {
	k, err := loadKnown(d, session)
	if err != nil {
		return
	}
	if _, ok := k.Workspaces[id]; !ok {
		return
	}
	delete(k.Workspaces, id)
	if k.Last == id {
		k.Last = ""
	}
	if err := saveKnown(d, session, k); err != nil {
		fmt.Fprintf(d.stderr, "hotbed: could not forget %s's workspace %s: %v\n", session, id, err)
	}
}

func saveKnown(d deps, session string, k known) error {
	b, err := json.Marshal(k)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(knownFile(d, session)), 0o755); err != nil {
		return err
	}
	return os.WriteFile(knownFile(d, session), b, 0o644)
}
