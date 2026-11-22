package main

import (
	"context"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

type workspaceCmd struct {
	Ls      workspaceLsCmd      `cmd:"" help:"List a session's workspaces, or every session's."`
	Show    workspaceShowCmd    `cmd:"" help:"Show a session's workspace of a repo, as its worker has it now."`
	Files   workspaceFilesCmd   `cmd:"" help:"List the files in a session's workspace of a repo, and how each differs from its base."`
	File    workspaceFileCmd    `cmd:"" help:"Print a file in a session's workspace of a repo, with the lines it changed against its base."`
	Release workspaceReleaseCmd `cmd:"" help:"Release a session's workspace of a repo: stop its commands and delete it. The session goes on; hotbed add gives it a new one."`
}

type workspaceLsCmd struct {
	controllerClient
	sessionFlag
	All bool `help:"Every session's workspaces."`
}

func (c *workspaceLsCmd) Exec(ctx context.Context, d deps) (any, error) {
	var session string
	if !c.All {
		var err error
		if session, err = c.sessionFlag.resolve(ctx, d); err != nil {
			return nil, err
		}
	}
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	return cl.workspaces.ListWorkspaces(ctx, &pb.ListWorkspacesRequest{SessionId: session})
}

// workspaceOfRepo is the flags that name a session's workspace of a repo.
type workspaceOfRepo struct {
	controllerClient
	sessionFlag
	repoFlag
}

// locate finds the workspace the flags name, and its worker.
func (f workspaceOfRepo) locate(ctx context.Context, d deps) (*pb.Workspace, workerClients, error) {
	session, err := f.sessionFlag.resolve(ctx, d)
	if err != nil {
		return nil, workerClients{}, err
	}
	repo, err := f.resolveRepo(ctx, d, session, f.Repo, "")
	if err != nil {
		return nil, workerClients{}, err
	}
	return f.controllerClient.locate(ctx, d, session, repo)
}

type workspaceShowCmd struct{ workspaceOfRepo }

func (c *workspaceShowCmd) Exec(ctx context.Context, d deps) (any, error) {
	ws, _, err := c.locate(ctx, d)
	return ws, err
}

type workspaceFilesCmd struct{ workspaceOfRepo }

func (c *workspaceFilesCmd) Exec(ctx context.Context, d deps) (any, error) {
	ws, w, err := c.locate(ctx, d)
	if err != nil {
		return nil, err
	}
	return w.calls.ListFiles(ctx, &pb.ListFilesRequest{WorkspaceId: ws.Id})
}

type workspaceFileCmd struct {
	workspaceOfRepo
	Path string `arg:"" help:"The file's path in the checkout, such as src/main.rs."`
}

func (c *workspaceFileCmd) Exec(ctx context.Context, d deps) (any, error) {
	ws, w, err := c.locate(ctx, d)
	if err != nil {
		return nil, err
	}
	return w.calls.GetFile(ctx, &pb.GetFileRequest{WorkspaceId: ws.Id, Path: c.Path})
}

type workspaceReleaseCmd struct{ workspaceOfRepo }

type releaseResult struct {
	ID   string `json:"id"`
	Repo string `json:"repo"`
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
	forget(d, ws.SessionId, ws.RepoId)
	return releaseResult{ID: ws.Id, Repo: ws.RepoId}, nil
}
