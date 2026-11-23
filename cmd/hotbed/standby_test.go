package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/controller"
	"github.com/semistrict/hotbed/internal/setup"
)

// fakeVM is a worker's VM as its host's supervisor runs it: stopping it
// halts the worker, starting it starts the worker again on its state.
type fakeVM struct {
	nd               *node
	started, stopped chan struct{}
	// booting, when set, holds Start until it is closed: a slow boot.
	booting chan struct{}
}

func (vm *fakeVM) Running(context.Context) bool { return !vm.nd.halted.Load() }

func (vm *fakeVM) Start(ctx context.Context) error {
	if vm.booting != nil {
		select {
		case <-vm.booting:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	vm.nd.start()
	select {
	case vm.started <- struct{}{}:
	default:
	}
	return nil
}

func (vm *fakeVM) Stop(context.Context) error {
	vm.nd.halt()
	select {
	case vm.stopped <- struct{}{}:
	default:
	}
	return nil
}

// standbyReadyTransport signals onReady once the first Standby response frame
// is read, guaranteeing the controller has registered s.power.hold and run its
// initial settlePower before the test advances the clock (board 9357).
type standbyReadyTransport struct {
	next    http.RoundTripper
	onReady func()
	once    sync.Once
}

func (t *standbyReadyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(r.URL.Path, "/Standby") {
		resp.Body = &firstReadBody{ReadCloser: resp.Body, onRead: func() { t.once.Do(t.onReady) }}
	}
	return resp, nil
}

type firstReadBody struct {
	io.ReadCloser
	onRead func()
}

func (b *firstReadBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.onRead()
	}
	return n, err
}

// supervise runs the worker's host's supervisor (hotbed worker supervise)
// over vm until the test ends, waiting until its first Standby call has
// registered on the controller so later clock advances cannot race hold().
func (c *cluster) supervise(vm *fakeVM, sleepAfter time.Duration) {
	c.t.Helper()
	ready := make(chan struct{})
	base := c.tn.Client(laptop)
	hc := &http.Client{Transport: &standbyReadyTransport{
		next:    base.Transport,
		onReady: func() { close(ready) },
	}}
	host, _, err := api.Controller(strings.TrimPrefix(c.ctrlSrv.URL, "http://"), hc)
	if err != nil {
		c.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(c.t.Context())
	var wg sync.WaitGroup
	wg.Go(func() { setup.Standby(ctx, host, vm.nd.peer.Hostname, sleepAfter, vm, slog.Default()) })
	c.t.Cleanup(func() { cancel(); wg.Wait() })
	within(c.t, "the supervisor to register with the controller", ready)
}

// within waits for ch, failing after a while.
func within(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// A worker whose host supervises it is powered off once it has had no work
// for its sleep-after, shows as asleep, and a run placed on it wakes it and
// runs there, in the session's workspace, without the client doing more
// than run.
func TestIdleNodeSleepsAndARunWakesIt(t *testing.T) {
	c := newCluster(t)
	origin := upstream(t)
	c.addRepo(origin)
	const S = "drowsy-dormouse"
	c.ok(nil, "session", "start", S)
	agent := filepath.Join(t.TempDir(), "agent")
	gitIn(t, filepath.Dir(agent), "clone", "--quiet", origin, agent)
	t.Chdir(agent)

	nd := c.nodes[0]
	vm := &fakeVM{nd: nd, started: make(chan struct{}, 1), stopped: make(chan struct{}, 1)}
	c.supervise(vm, 30*time.Minute)

	if node, _ := c.ranOn(S, "one\n", "--", "echo", "one"); node != "asahimbp" {
		t.Fatalf("ran on %s, want asahimbp", node)
	}
	// Wait for the command-exit event to reach the controller before advancing
	// the clock, so a late PushEvents cannot reset power.worked at t=29m.
	c.converged()
	// The sessions clients used on it go with the next heartbeat.
	c.heartbeat()

	// 29 minutes on, it is up.
	c.clock.advance(29 * time.Minute)
	c.heartbeat()
	select {
	case <-vm.stopped:
		t.Fatal("stopped 29 minutes after its last run, want 30")
	case <-time.After(time.Second):
	}

	// 31 minutes on, it sleeps.
	c.clock.advance(2 * time.Minute)
	c.heartbeat()
	within(t, "the idle node to be powered off", vm.stopped)
	if code, out, stderr := c.hotbed("nodes"); code != 0 || out != "asahimbp          arm64   8 cpus  asleep\n" {
		t.Fatalf("nodes: exit %d, stdout %q, stderr %q; want asahimbp asleep", code, out, stderr)
	}

	// A run wakes it, and goes there once it is up.
	if node, _ := c.ranOn(S, "two\n", "--", "echo", "two"); node != "asahimbp" {
		t.Fatalf("ran on %s, want asahimbp", node)
	}
	within(t, "the node to be powered on", vm.started)
	if code, out, stderr := c.hotbed("nodes"); code != 0 || out != "asahimbp          arm64   8 cpus  online\n" {
		t.Fatalf("nodes: exit %d, stdout %q, stderr %q; want asahimbp online", code, out, stderr)
	}
}

// A node that takes longer to boot than the controller waits in one call
// has hotbed run call again (NODE_WAKING), saying so, until it is up.
func TestRunWaitsForANodeSlowToWake(t *testing.T) {
	c := newClusterWith(t, clusterOpts{server: func(cfg *controller.ServerConfig) { cfg.WakeWait = time.Second }})
	origin := upstream(t)
	c.addRepo(origin)
	const S = "slow-sloth"
	c.ok(nil, "session", "start", S)
	agent := filepath.Join(t.TempDir(), "agent")
	gitIn(t, filepath.Dir(agent), "clone", "--quiet", origin, agent)
	t.Chdir(agent)

	nd := c.nodes[0]
	vm := &fakeVM{nd: nd, started: make(chan struct{}, 1), stopped: make(chan struct{}, 1), booting: make(chan struct{})}
	c.supervise(vm, 30*time.Minute)
	c.ranOn(S, "one\n", "--", "echo", "one")
	// Wait for the command-exit event to reach the controller before advancing
	// the clock, so a late PushEvents cannot reset power.worked at t=31m (board 9357).
	c.converged()
	c.heartbeat()
	c.clock.advance(31 * time.Minute)
	c.heartbeat()
	within(t, "the idle node to be powered off", vm.stopped)

	// Hold the slow boot until the CLI has observed NODE_WAKING and printed its
	// retry diagnostic, so scheduling delays before placement cannot let the VM
	// finish booting before the first WakeWait expires (board 9590).
	const wantWake = "hotbed: asahimbp was asleep and is still waking up; waiting up to 5m0s for it\n"
	var releaseOnce sync.Once
	releaseBoot := func() { releaseOnce.Do(func() { close(vm.booting) }) }

	stderr := &signalWriter{want: wantWake, done: make(chan struct{})}
	type runResult struct {
		code   int
		stdout string
	}
	done := make(chan runResult, 1)
	finished := make(chan struct{})
	t.Cleanup(func() {
		releaseBoot()
		<-finished
	})
	go func() {
		defer close(finished)
		var stdout bytes.Buffer
		d := c.cli
		d.stdin, d.stdout, d.stderr = strings.NewReader(""), &stdout, stderr
		code := run(t.Context(), []string{"run", "-s", S, "-r", "app", "--", "echo", "two"}, d)
		done <- runResult{code: code, stdout: stdout.String()}
	}()

	select {
	case <-stderr.done:
	case r := <-done:
		t.Fatalf("run exited before reporting NODE_WAKING: exit %d, stdout %q, stderr %q", r.code, r.stdout, stderr.String())
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for NODE_WAKING diagnostic; stderr so far: %q", stderr.String())
	}
	releaseBoot()

	var r runResult
	select {
	case r = <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for run to finish after wake; stderr %q", stderr.String())
	}
	errOut := stderr.String()
	if r.code != 0 || r.stdout != "two\n" {
		t.Fatalf("run after wake: exit %d, stdout %q (want %q), stderr %q", r.code, r.stdout, "two\n", errOut)
	}
	_, rest, ok := strings.Cut(errOut, " on ")
	if !ok {
		t.Fatalf("run said nowhere where it ran: %q", errOut)
	}
	node, _, _ := strings.Cut(rest, " ")
	if node != "asahimbp" {
		t.Fatalf("ran on %s, want asahimbp", node)
	}
	if !strings.Contains(errOut, wantWake) {
		t.Fatalf("stderr %q says nothing of waiting for asahimbp to wake", errOut)
	}
}
