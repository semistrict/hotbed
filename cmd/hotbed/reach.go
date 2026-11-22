package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
)

// The controller says where a session's workspaces are; the CLI then
// syncs, runs commands and reads their output on the workspace's worker
// itself, so bundles and logs never detour through the controller, and work
// goes on while the controller is out of reach.

// workerClients is the clients that reach one workspace's worker: calls for unary
// calls, stream for streams, which have no deadline.
type workerClients struct {
	calls, stream hotbedv1connect.WorkspaceServiceClient
}

// reach is the worker at addr; with no address (a controller from before
// workers took calls), the controller, which forwards them.
func (f controllerFlag) reach(d deps, addr string) (workerClients, error) {
	if addr == "" {
		cl, err := f.clients(d.http)
		if err != nil {
			return workerClients{}, err
		}
		st, err := f.clients(d.stream)
		return workerClients{calls: cl.workspaces, stream: st.workspaces}, err
	}
	calls, err := api.Dial(addr, api.WorkerPort, d.http)
	if err != nil {
		return workerClients{}, err
	}
	stream, err := api.Dial(addr, api.WorkerPort, d.stream)
	if err != nil {
		return workerClients{}, err
	}
	return workerClients{calls: hotbedv1connect.NewWorkspaceServiceClient(calls), stream: hotbedv1connect.NewWorkspaceServiceClient(stream)}, nil
}

// locate finds the session's workspace of a repo (or with that ID) and its
// worker, and returns the workspace as its worker has it now. With the
// controller out of reach, it goes by where this machine last found the
// workspace.
func (f controllerFlag) locate(ctx context.Context, d deps, session, repo string) (*pb.Workspace, workerClients, error) {
	cl, err := f.clients(d.http)
	if err != nil {
		return nil, workerClients{}, err
	}
	var found *pb.Workspace
	list, err := cl.workspaces.ListWorkspaces(ctx, &pb.ListWorkspacesRequest{SessionId: session})
	switch {
	case err == nil:
		for _, w := range list.Workspaces {
			if w.RepoId == repo || w.Id == repo {
				found = w
				break
			}
		}
		if found == nil {
			return nil, workerClients{}, api.Errorf(api.CodeNotFound, "session %s has no workspace of %s", session, repo).
				WithSuggestion(fmt.Sprintf("hotbed add %s %s (with --sync DIR to follow a local checkout)", session, repo))
		}
	case api.FromConnect(err).Code == api.CodeControllerUnreachable:
		known, cerr := loadKnown(d, session)
		if cerr != nil || known[repo].ID == "" {
			return nil, workerClients{}, err
		}
		fmt.Fprintf(d.stderr, "hotbed: the controller is out of reach; using %s on its worker\n", known[repo].ID)
		found = &pb.Workspace{Id: known[repo].ID, NodeAddr: known[repo].NodeAddr}
	default:
		return nil, workerClients{}, err
	}
	w, err := f.reach(d, found.NodeAddr)
	if err != nil {
		return nil, workerClients{}, err
	}
	ws, err := w.calls.GetWorkspace(ctx, &pb.GetWorkspaceRequest{Id: found.Id})
	if err != nil {
		return nil, workerClients{}, err
	}
	ws.NodeAddr = found.NodeAddr
	remember(d, session, ws)
	return ws, w, nil
}

// locateCommand finds a command of the session (or, for reading output,
// a base's freshen, which belongs to no session) and its worker.
func (f controllerFlag) locateCommand(ctx context.Context, d deps, session, id string, freshens bool) (*pb.Command, workerClients, error) {
	cl, err := f.clients(d.http)
	if err != nil {
		return nil, workerClients{}, err
	}
	cmd, err := cl.workspaces.GetCommand(ctx, &pb.GetCommandRequest{Id: id})
	if err != nil && api.FromConnect(err).Code == api.CodeControllerUnreachable {
		return f.askKnownWorkers(ctx, d, session, id, err)
	}
	if err != nil {
		return nil, workerClients{}, err
	}
	if cmd.SessionId != session && !(freshens && cmd.SessionId == "") {
		return nil, workerClients{}, api.Errorf(api.CodeNotFound, "session %s has no command %s", session, id)
	}
	w, err := f.reach(d, cmd.NodeAddr)
	return cmd, w, err
}

// askKnownWorkers looks for a command on the workers this machine last
// found the session's workspaces on.
func (f controllerFlag) askKnownWorkers(ctx context.Context, d deps, session, id string, unreachable error) (*pb.Command, workerClients, error) {
	known, err := loadKnown(d, session)
	if err != nil {
		return nil, workerClients{}, unreachable
	}
	for _, k := range known {
		if k.NodeAddr == "" {
			continue
		}
		w, err := f.reach(d, k.NodeAddr)
		if err != nil {
			continue
		}
		if cmd, err := w.calls.GetCommand(ctx, &pb.GetCommandRequest{Id: id}); err == nil && cmd.SessionId == session {
			return cmd, w, nil
		}
	}
	return nil, workerClients{}, unreachable
}

// known is where this machine last found a session's workspaces, by repo.
type known map[string]spot

type spot struct {
	ID       string `json:"id"`
	NodeAddr string `json:"nodeAddr"`
}

func knownFile(d deps, session string) string {
	return filepath.Join(d.cache, "sessions", session+".workspaces.json")
}

func loadKnown(d deps, session string) (known, error) {
	b, err := os.ReadFile(knownFile(d, session))
	if err != nil {
		return nil, err
	}
	k := known{}
	return k, json.Unmarshal(b, &k)
}

// remember notes where a session's workspace is, for when the controller
// is out of reach.
func remember(d deps, session string, ws *pb.Workspace) {
	k, err := loadKnown(d, session)
	if err != nil {
		k = known{}
	}
	if k[ws.RepoId].ID == ws.Id && k[ws.RepoId].NodeAddr == ws.NodeAddr {
		return
	}
	k[ws.RepoId] = spot{ID: ws.Id, NodeAddr: ws.NodeAddr}
	b, err := json.Marshal(k)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(knownFile(d, session)), 0o755)
	}
	if err == nil {
		err = os.WriteFile(knownFile(d, session), b, 0o644)
	}
	if err != nil {
		fmt.Fprintf(d.stderr, "hotbed: could not note where %s is: %v\n", ws.Id, err)
	}
}
