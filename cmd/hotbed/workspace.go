package main

import (
	"context"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

type workspaceCmd struct {
	Ls      workspaceLsCmd      `cmd:"" help:"List a session's workspaces, or every session's."`
	Show    workspaceShowCmd    `cmd:"" help:"Show a session's workspace, as its worker has it now."`
	Files   workspaceFilesCmd   `cmd:"" help:"List the files of every repo in a session's workspace (each path starts with its repo), and how each differs from its base."`
	File    workspaceFileCmd    `cmd:"" help:"Print a file in a session's workspace, by its path below /work (hotbed/go.mod), with the lines it changed against its base."`
	Release workspaceReleaseCmd `cmd:"" help:"Release a session's workspace: stop its commands and delete it. The session goes on; its next run gets a new one."`
}

type workspaceLsCmd struct {
	controllerClient
	sessionFlag
	Project string `short:"p" help:"Every live workspace in this project."`
	All     bool   `help:"Every session's workspaces."`
}

func (c *workspaceLsCmd) Exec(ctx context.Context, d deps) (any, error) {
	var session string
	if !c.All && c.Project == "" {
		var err error
		if session, err = c.sessionFlag.resolve(ctx, d); err != nil {
			return nil, err
		}
	}
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.workspaces.ListWorkspaces(ctx, &pb.ListWorkspacesRequest{SessionId: session, Project: c.Project})
}

// workspaceOfSession is the flags that name a session's workspace.
type workspaceOfSession struct {
	controllerClient
	sessionFlag
	Node string `help:"The workspace on this node (host name), when the session has some on several."`
}

// locate finds the workspace the flags name, and its worker.
func (f workspaceOfSession) locate(ctx context.Context, d deps) (*pb.Workspace, workerClients, error) {
	session, err := f.sessionFlag.resolve(ctx, d)
	if err != nil {
		return nil, workerClients{}, err
	}
	return f.controllerClient.locate(ctx, d, session, f.Node)
}

type workspaceShowCmd struct{ workspaceOfSession }

func (c *workspaceShowCmd) Exec(ctx context.Context, d deps) (any, error) {
	ws, _, err := c.locate(ctx, d)
	return ws, err
}

type workspaceFilesCmd struct{ workspaceOfSession }

func (c *workspaceFilesCmd) Exec(ctx context.Context, d deps) (any, error) {
	ws, w, err := c.locate(ctx, d)
	if err != nil {
		return nil, err
	}
	return w.calls.ListFiles(ctx, &pb.ListFilesRequest{WorkspaceId: ws.Id})
}

type workspaceFileCmd struct {
	workspaceOfSession
	Path string `arg:"" help:"The file's path below /work, starting with its repo, such as ratel/src/main.rs."`
}

func (c *workspaceFileCmd) Exec(ctx context.Context, d deps) (any, error) {
	ws, w, err := c.locate(ctx, d)
	if err != nil {
		return nil, err
	}
	return w.calls.GetFile(ctx, &pb.GetFileRequest{WorkspaceId: ws.Id, Path: c.Path})
}

type workspaceReleaseCmd struct{ workspaceOfSession }

type releaseResult struct {
	ID         string `json:"id"`
	Project    string `json:"project"`
	Node       string `json:"node"`
	CockpitURL string `json:"cockpitUrl"`
}

// Exec releases the workspace through the controller, which keeps the
// session's account of its workspaces.
func (c *workspaceReleaseCmd) Exec(ctx context.Context, d deps) (any, error) {
	ws, _, err := c.locate(ctx, d)
	if err != nil {
		return nil, err
	}
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	if _, err := cl.workspaces.ReleaseWorkspace(ctx, &pb.ReleaseWorkspaceRequest{Id: ws.Id}); err != nil {
		return nil, err
	}
	forget(d, ws.SessionId, ws.Id)
	return releaseResult{ID: ws.Id, Project: ws.Project, Node: ws.Node, CockpitURL: d.cockpit.workspace(ws.Id)}, nil
}
