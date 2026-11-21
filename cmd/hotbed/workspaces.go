package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/gitops"
)

type wsCmd struct {
	New wsNewCmd `cmd:"" help:"Fork a repo's newest warm base into a new workspace."`
	Ls  wsLsCmd  `cmd:"" help:"List workspaces."`
	Rm  wsRmCmd  `cmd:"" help:"Release a workspace: stop its commands, delete its fork."`
}

type wsNewCmd struct {
	controllerFlag
	Repo   string `arg:"" help:"The repo's name, from hotbed repo ls."`
	Agent  string `env:"HOTBED_AGENT" help:"Who works in it, for the cockpit (default: this machine)."`
	Branch string `help:"The branch the agent works on, for the cockpit."`
	Node   string `help:"Put it on this node (host name) instead of the one with the most free memory."`
}

func (c *wsNewCmd) Exec(ctx context.Context, d deps) (any, error) {
	client, err := c.client(d.http)
	if err != nil {
		return nil, err
	}
	var ws api.Workspace
	err = client.Do(ctx, http.MethodPost, "/v1/workspaces", api.CreateWorkspace{Repo: c.Repo, Agent: c.Agent, Branch: c.Branch, Node: c.Node}, &ws)
	return ws, err
}

type wsLsCmd struct {
	controllerFlag
}

func (c *wsLsCmd) Exec(ctx context.Context, d deps) (any, error) {
	client, err := c.client(d.http)
	if err != nil {
		return nil, err
	}
	var out []api.Workspace
	err = client.Do(ctx, http.MethodGet, "/v1/workspaces", nil, &out)
	return map[string]any{"workspaces": out}, err
}

type wsRmCmd struct {
	controllerFlag
	Workspace string `arg:"" help:"The workspace's ID."`
}

func (c *wsRmCmd) Exec(ctx context.Context, d deps) (any, error) {
	client, err := c.client(d.http)
	if err != nil {
		return nil, err
	}
	return map[string]string{"released": c.Workspace}, client.Do(ctx, http.MethodDelete, "/v1/workspaces/"+c.Workspace, nil, nil)
}

type syncCmd struct {
	controllerFlag
	Workspace string `arg:"" help:"The workspace's ID."`
	Dir       string `default:"." type:"path" help:"A directory in the checkout to send."`
}

func (c *syncCmd) Exec(ctx context.Context, d deps) (any, error) {
	client, err := c.client(d.http)
	if err != nil {
		return nil, err
	}
	return sync(ctx, client, c.Workspace, c.Dir)
}

// sync sends the checkout's state to a workspace, unless the workspace
// already has exactly this tree.
func sync(ctx context.Context, client *api.Client, wsID, dir string) (api.SyncResult, error) {
	ws, err := workspace(ctx, client, wsID)
	if err != nil {
		return api.SyncResult{}, err
	}
	snap, err := gitops.Snapshot(ctx, dir)
	if err != nil {
		return api.SyncResult{}, err
	}
	if snap.Tree == ws.Tree {
		return api.SyncResult{Workspace: wsID, Head: ws.Head, Tree: ws.Tree, FilesChanged: ws.FilesChanged, Skipped: true}, nil
	}
	var bundle bytes.Buffer
	if err := gitops.Bundle(ctx, snap, ws.BaseCommit, &bundle); err != nil {
		return api.SyncResult{}, err
	}
	q := url.Values{"commit": {snap.Commit}, "tree": {snap.Tree}}
	resp, err := client.Stream(ctx, http.MethodPost, "/v1/workspaces/"+wsID+"/sync?"+q.Encode(), &bundle, "application/x-git-bundle")
	if err != nil {
		return api.SyncResult{}, err
	}
	defer resp.Body.Close()
	var out api.SyncResult
	if err := decodeJSON(resp.Body, &out); err != nil {
		return api.SyncResult{}, err
	}
	return out, nil
}

func workspace(ctx context.Context, client *api.Client, id string) (api.Workspace, error) {
	var w api.Workspace
	err := client.Do(ctx, http.MethodGet, "/v1/workspaces/"+id, nil, &w)
	return w, err
}

type execCmd struct {
	controllerFlag
	Workspace string   `arg:"" help:"The workspace's ID."`
	Args      []string `arg:"" passthrough:"" help:"The command, after --."`
	Dir       string   `default:"." type:"path" help:"A directory in the checkout to sync first."`
	NoSync    bool     `help:"Run without syncing first."`
	Detach    bool     `help:"Return once the command is queued; it runs until it ends or hotbed stop. For dev servers."`
	CPUWeight int      `default:"100" help:"Share of CPU when CPUs are contended (cgroup cpu.weight, 1-10000)."`
	MemLow    string   `default:"0" help:"Memory guaranteed to the command (cgroup memory.low), such as 2G; admission waits until it fits."`
	MemHigh   string   `default:"0" help:"Memory past which the command is throttled and swapped (memory.high), such as 8G; 0 for none."`
	Port      int      `help:"Port a dev server listens on, for its URL in the cockpit."`
	Env       []string `help:"KEY=VALUE for the command. Repeatable."`
	Kind      string   `enum:",build,test,check,server,shell" default:"" help:"What it is, for history: build, test, check, server or shell (default: guessed)."`
}

// Raw syncs, runs the command, and streams its output to stdout. An
// attached command is stopped when hotbed exec is interrupted.
func (c *execCmd) Raw(ctx context.Context, d deps) (int, bool, error) {
	// kong's passthrough keeps the -- that ends hotbed's own flags.
	if len(c.Args) > 0 && c.Args[0] == "--" {
		c.Args = c.Args[1:]
	}
	if len(c.Args) == 0 {
		return 0, false, api.Errorf(api.CodeInvalidArgs, "no command to run").WithSuggestion("hotbed exec WS -- cargo test")
	}
	low, err := parseSize(c.MemLow)
	if err != nil {
		return 0, false, err
	}
	high, err := parseSize(c.MemHigh)
	if err != nil {
		return 0, false, err
	}
	client, err := c.client(d.http)
	if err != nil {
		return 0, false, err
	}
	stream, err := c.client(d.stream)
	if err != nil {
		return 0, false, err
	}
	if !c.NoSync {
		res, err := sync(ctx, client, c.Workspace, c.Dir)
		if err != nil {
			return 0, false, err
		}
		if !res.Skipped {
			fmt.Fprintf(d.stderr, "hotbed: synced %d changed files (%s) to %s\n", res.FilesChanged, size(res.BundleBytes), c.Workspace)
		}
	}
	var cmd api.Command
	err = client.Do(ctx, http.MethodPost, "/v1/workspaces/"+c.Workspace+"/commands", api.RunCommand{
		Args: c.Args, Env: c.Env, Kind: c.Kind, Attached: !c.Detach, Port: c.Port,
		Limits: api.CommandLimits{CPUWeight: c.CPUWeight, MemoryLowBytes: low, MemoryHighBytes: high},
	}, &cmd)
	if err != nil {
		return 0, false, err
	}
	if c.Detach {
		writeEnvelope(d.stdout, success("exec", cmd))
		return exitOK, true, nil
	}
	fmt.Fprintf(d.stderr, "hotbed: %s on %s (hotbed stop %s)\n", cmd.ID, cmd.NodeID, cmd.ID)
	code, err := follow(ctx, client, stream, cmd.ID, d.stdout)
	if ctx.Err() != nil {
		// Interrupted: an attached command stops with its client.
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := client.Do(stopCtx, http.MethodPost, "/v1/commands/"+cmd.ID+"/stop", nil, nil); err != nil {
			fmt.Fprintf(d.stderr, "hotbed: could not stop %s: %v\n", cmd.ID, err)
		}
		return 130, true, nil
	}
	if err != nil {
		return 0, true, err
	}
	return code, true, nil
}

// Exec is never called: exec is a rawCommand.
func (c *execCmd) Exec(context.Context, deps) (any, error) {
	return nil, errors.New("exec runs raw")
}

// follow copies a command's output to w until it ends, reconnecting if the
// stream drops, and returns its exit status.
func follow(ctx context.Context, client, stream *api.Client, id string, w io.Writer) (int, error) {
	var offset int64
	for {
		resp, err := stream.Stream(ctx, http.MethodGet, fmt.Sprintf("/v1/commands/%s/log?follow=1&offset=%d", id, offset), nil, "")
		if err != nil {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			return 0, err
		}
		n, _ := io.Copy(w, resp.Body)
		resp.Body.Close()
		offset += n
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		var c api.Command
		if err := client.Do(ctx, http.MethodGet, "/v1/commands/"+id, nil, &c); err != nil {
			return 0, err
		}
		if c.State.Live() {
			continue // the stream dropped while it runs: pick up where it left off
		}
		switch {
		case c.ExitCode != nil:
			return *c.ExitCode, nil
		case c.State == api.CommandCancelled:
			return 0, api.Errorf(api.CodeInternal, "%s was cancelled: %s", id, deref(c.Reason))
		case strings.HasPrefix(deref(c.Reason), "killed by signal "):
			// Report it the way a shell would: 128 plus the signal.
			sig, _ := strconv.Atoi(strings.TrimPrefix(deref(c.Reason), "killed by signal "))
			return 128 + sig, nil
		default:
			return 0, api.Errorf(api.CodeInternal, "%s failed: %s", id, deref(c.Reason))
		}
	}
}

type psCmd struct {
	controllerFlag
}

func (c *psCmd) Exec(ctx context.Context, d deps) (any, error) {
	client, err := c.client(d.http)
	if err != nil {
		return nil, err
	}
	var out []api.Command
	err = client.Do(ctx, http.MethodGet, "/v1/commands", nil, &out)
	return map[string]any{"commands": out}, err
}

type logsCmd struct {
	controllerFlag
	Command string `arg:"" help:"The command's ID."`
	Follow  bool   `short:"f" help:"Keep printing until the command ends."`
}

// Raw prints the log as is.
func (c *logsCmd) Raw(ctx context.Context, d deps) (int, bool, error) {
	stream, err := c.client(d.stream)
	if err != nil {
		return 0, false, err
	}
	q := "/v1/commands/" + c.Command + "/log"
	if c.Follow {
		q += "?follow=1"
	}
	resp, err := stream.Stream(ctx, http.MethodGet, q, nil, "")
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()
	if _, err := io.Copy(d.stdout, resp.Body); err != nil && ctx.Err() == nil {
		return 0, true, err
	}
	return exitOK, true, nil
}

func (c *logsCmd) Exec(context.Context, deps) (any, error) { return nil, errors.New("logs runs raw") }

type stopCmd struct {
	controllerFlag
	Command string `arg:"" help:"The command's ID."`
}

func (c *stopCmd) Exec(ctx context.Context, d deps) (any, error) {
	client, err := c.client(d.http)
	if err != nil {
		return nil, err
	}
	if err := client.Do(ctx, http.MethodPost, "/v1/commands/"+c.Command+"/stop", nil, nil); err != nil {
		return nil, err
	}
	var cmd api.Command
	err = client.Do(ctx, http.MethodGet, "/v1/commands/"+c.Command, nil, &cmd)
	return cmd, err
}

// parseSize reads 512M, 2G, 1.5G or a plain byte count.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	mult := int64(1)
	for suffix, m := range map[string]int64{"K": 1 << 10, "M": 1 << 20, "G": 1 << 30, "T": 1 << 40} {
		if v, ok := strings.CutSuffix(strings.TrimSuffix(s, "B"), suffix); ok && v != s {
			s, mult = v, m
			break
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0, api.Errorf(api.CodeInvalidArgs, "size %q is not like 512M or 2G", s)
	}
	return int64(f * float64(mult)), nil
}

func size(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
