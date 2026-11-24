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
	"time"

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
	if b.Number == d.build {
		return upgradeResult{Build: b.Number}, nil
	}
	if err := install(ctx, c.Controller, d, b); err != nil {
		return nil, err
	}
	return upgradeResult{Build: b.Number, Upgraded: true, From: d.build}, nil
}

type adminCmd struct {
	Upgrade adminUpgradeCmd `cmd:"" help:"Have the controller check CI for a newer build now and refresh its release metadata (workers follow within seconds)."`
}

type adminUpgradeCmd struct {
	controllerClient
	Wait time.Duration `default:"2m" help:"How long to wait for the restarted controller to answer on the new build (0: return as it restarts)."`
}

type adminUpgradeResult struct {
	Build      int64 `json:"build"`
	Upgraded   bool  `json:"upgraded"`
	From       int64 `json:"from,omitempty"`
	Restarting bool  `json:"restarting"`
}

func (c *adminUpgradeCmd) Exec(ctx context.Context, d deps) (any, error) {
	// Downloading every platform's binary from GitHub can exceed the short RPC
	// timeout on d.http, so call over d.stream.
	cl, err := c.clients(d.stream)
	if err != nil {
		return nil, err
	}
	res, err := cl.cluster.UpgradeController(ctx, &pb.UpgradeControllerRequest{})
	if err != nil {
		return nil, err
	}
	upgraded := res.Restarting || res.Build > res.From
	out := adminUpgradeResult{
		Build:      res.Build,
		Upgraded:   upgraded,
		Restarting: res.Restarting,
	}
	if upgraded {
		out.From = res.From
	}
	if res.Restarting && c.Wait > 0 {
		if err := awaitControllerBuild(ctx, cl, res.Build, c.Wait); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// awaitControllerBuild returns once the controller answers on build n, as
// it does once it has restarted into it: until then the old process answers
// on the old build, and then nothing does for a moment.
func awaitControllerBuild(parent context.Context, cl clients, n int64, wait time.Duration) error {
	ctx, cancel := context.WithTimeout(parent, wait)
	defer cancel()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	var last string
	timedOut := func() error {
		if err := parent.Err(); err != nil {
			return err
		}
		return api.Errorf(api.CodeControllerUnreachable, "the controller did not answer on build %d within %s (%s)", n, wait, last).
			WithSuggestion("check that `hotbed serve` restarted on the controller, and its log")
	}
	for {
		b, err := cl.cluster.GetBuild(ctx, &pb.GetBuildRequest{Os: runtime.GOOS, Arch: runtime.GOARCH})
		switch {
		case err == nil && b.Number == n:
			return nil
		case err == nil:
			last = fmt.Sprintf("it still answers on build %d", b.Number)
		case ctx.Err() != nil:
			return timedOut()
		case api.FromConnect(err).Retryable:
			last = api.FromConnect(err).Message
		default:
			return err
		}
		select {
		case <-ctx.Done():
			return timedOut()
		case <-tick.C:
		}
	}
}

// install puts the controller's build b over this client's binary, then has
// the new binary install its skills.
func install(ctx context.Context, controllerAddr string, d deps, b *pb.Build) error {
	exe, err := d.exe()
	if err != nil {
		return err
	}
	// A link (~/.local/bin/hotbed) is replaced where it points.
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	base, err := api.ControllerBaseURL(controllerAddr)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+b.Path, nil)
	if err != nil {
		return err
	}
	if tok := controllerAuthToken(controllerAddr); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := d.stream.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return api.Errorf(api.CodeInternal, "GET %s: %s", req.URL, resp.Status)
	}
	if err := update.Install(resp.Body, b.Sha256, exe); err != nil {
		return err
	}
	return d.reexec(exe, "skills", "install")
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
// client runs another CI build. Its failure is said, never the command's.
func followController(ctx context.Context, d deps, addr string, seen int64) {
	if d.build == 0 || seen == 0 || seen == d.build {
		return
	}
	cl, err := controllerClient{Controller: addr}.clients(d.http)
	if err == nil {
		var b *pb.Build
		if b, err = cl.cluster.GetBuild(ctx, &pb.GetBuildRequest{Os: runtime.GOOS, Arch: runtime.GOARCH}); err == nil {
			err = install(ctx, addr, d, b)
		}
		if err == nil {
			fmt.Fprintf(d.stderr, "hotbed: upgraded to build %d, the controller's (was build %d)\n", b.Number, d.build)
			return
		}
	}
	fmt.Fprintf(d.stderr, "hotbed: could not upgrade to build %d, the controller's: %v\n", seen, toAPIError(err))
}
