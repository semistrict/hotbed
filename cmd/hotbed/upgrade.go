package main

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync/atomic"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/update"
)

// A client follows the controller's build as workers do: from the
// controller, which keeps it. One on a CI build notices the controller's
// build on every answer (api.BuildHeader) and, after the command it
// ran, installs it over itself; a dev build is left alone, as someone
// building hotbed runs it. `hotbed upgrade` installs it whatever the client
// runs.

type upgradeCmd struct {
	controllerClient
}

type upgradeResult struct {
	Build    int64 `json:"build"`
	Upgraded bool  `json:"upgraded"`
	From     int64 `json:"from,omitempty"`
}

func (c *upgradeCmd) Exec(ctx context.Context, d deps) (any, error) {
	cl, err := c.clients(d.http)
	if err != nil {
		return nil, err
	}
	b, err := cl.cluster.GetBuild(ctx, &pb.GetBuildRequest{Os: runtime.GOOS, Arch: runtime.GOARCH})
	if err != nil {
		return nil, err
	}
	if d.build > 0 && d.build >= b.Number {
		return upgradeResult{Build: d.build}, nil
	}
	changed, err := c.install(ctx, d, b)
	if err != nil {
		return nil, err
	}
	if !changed {
		return upgradeResult{Build: b.Number}, nil
	}
	return upgradeResult{Build: b.Number, Upgraded: true, From: d.build}, nil
}

// install puts the controller's build b over this client's binary, then has
// the new binary install its skills. It returns changed=false without touching
// disk when the binary on disk already matches b.Sha256 (for instance, when a
// long-lived hotbed mcp or logs -f process started on an older build exits
// after another process already installed b).
func (f controllerClient) install(ctx context.Context, d deps, b *pb.Build) (bool, error) {
	exe, err := d.exe()
	if err != nil {
		return false, err
	}
	// A link (~/.local/bin/hotbed) is replaced where it points.
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return false, err
	}
	if update.Matches(exe, b.Sha256) {
		return false, nil
	}
	downloadURL := b.DownloadUrl
	fromController := downloadURL == ""
	if fromController {
		base, err := api.ControllerBaseURL(f.Controller)
		if err != nil {
			return false, err
		}
		downloadURL = base + b.Path
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return false, err
	}
	if fromController {
		if err := f.authorize(ctx, d, req.Header); err != nil {
			return false, err
		}
	}
	resp, err := d.stream.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, api.Errorf(api.CodeInternal, "GET %s: %s", req.URL, resp.Status)
	}
	if err := update.Install(resp.Body, b.Sha256, exe); err != nil {
		return false, err
	}
	if err := d.reexec(exe, "skills", "install"); err != nil {
		return false, err
	}
	return true, nil
}

// reexec runs a binary, its output dropped unless it fails.
func reexec(args ...string) error {
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %w: %s", args, err, out)
	}
	return nil
}

// buildSeen records the controller's build from its answers.
type buildSeen struct {
	base http.RoundTripper
	n    *atomic.Int64
}

func (b buildSeen) Unwrap() http.RoundTripper { return b.base }

func (b buildSeen) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := b.base.RoundTrip(r)
	if err == nil {
		if n, perr := strconv.ParseInt(resp.Header.Get(api.BuildHeader), 10, 64); perr == nil && n > 0 {
			b.n.Store(n)
		}
	}
	return resp, err
}

// watchBuild has d's clients record the controller's build in seen.
func watchBuild(d deps, seen *atomic.Int64) deps {
	wrap := func(c *http.Client) *http.Client {
		if c == nil {
			return nil
		}
		base := c.Transport
		if base == nil {
			base = http.DefaultTransport
		}
		w := *c
		w.Transport = buildSeen{base: base, n: seen}
		return &w
	}
	d.http, d.stream = wrap(d.http), wrap(d.stream)
	return d
}

// followController installs the controller's build at addr after a command, when this
// client runs an older CI build. Its failure is said, never the command's.
func followController(ctx context.Context, d deps, addr string, seen int64) {
	if d.build == 0 || seen == 0 || seen <= d.build {
		return
	}
	cc := controllerClient{Controller: addr, Getenv: d.getenv}
	cl, err := cc.clients(d.http)
	if err == nil {
		var b *pb.Build
		var changed bool
		if b, err = cl.cluster.GetBuild(ctx, &pb.GetBuildRequest{Os: runtime.GOOS, Arch: runtime.GOARCH}); err == nil {
			changed, err = cc.install(ctx, d, b)
		}
		if err == nil {
			if changed {
				fmt.Fprintf(d.stderr, "hotbed: upgraded to build %d, the controller's (was build %d)\n", b.Number, d.build)
			}
			return
		}
	}
	fmt.Fprintf(d.stderr, "hotbed: could not upgrade to build %d, the controller's: %v\n", seen, toAPIError(err))
}
