package cfbundle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// harnessScript is the harness's entry point, relative to the repo root.
var harnessScript = filepath.Join("cf-controller", "scripts", "harness.mjs")

// FindRoot returns the repo root: the nearest directory, from the working
// directory up, holding the cf-controller harness script.
func FindRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, harnessScript)); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not locate %s from the working directory", harnessScript)
		}
		dir = parent
	}
}

// HarnessOptions configures StartHarness. The zero value runs the checkout's
// bundle on a free port with the harness's own default tokens.
type HarnessOptions struct {
	// Root is the repo root. Empty finds it from the working directory.
	Root string
	// Assets is a directory the Worker serves as its cockpit static assets,
	// through the same path production uses (ui/dist, say). Empty serves none.
	Assets string
	// Port to listen on, on 127.0.0.1. Zero picks a free one.
	Port int
	// Owner, Name, Token and JoinToken override the harness's defaults when
	// not empty.
	Owner, Name, Token, JoinToken string
	// Persist is a directory the Worker's Durable Objects keep their state in,
	// so it outlives the harness. Empty keeps state in memory.
	Persist string
	// ReadyTimeout bounds the wait for the harness to come up. Zero is 30s.
	ReadyTimeout time.Duration
}

func (o HarnessOptions) args(bundleDir string) []string {
	args := []string{"--bundle", bundleDir}
	if o.Port != 0 {
		args = append(args, "--port", strconv.Itoa(o.Port))
	}
	for _, f := range []struct{ flag, value string }{
		{"--assets", o.Assets},
		{"--owner", o.Owner},
		{"--name", o.Name},
		{"--token", o.Token},
		{"--join-token", o.JoinToken},
		{"--persist", o.Persist},
	} {
		if f.value != "" {
			args = append(args, f.flag, f.value)
		}
	}
	return args
}

// Harness is a cf-controller Worker running in workerd (Miniflare), as a child
// process. Every Durable Object in it is reached through the X-Hotbed-Test-DO
// header, so one Harness serves many isolated clusters.
type Harness struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	done   chan struct{} // closed once the process has exited
	stderr *lockedBuffer
	url    string
	token  string
}

// StartHarness builds the cf-controller bundle if it is missing or stale, then
// starts the harness on it and waits for it to be ready. ctx bounds only the
// wait: the harness runs until Close, or until this process exits (its stdin
// closes, and it shuts down).
func StartHarness(ctx context.Context, opts HarnessOptions) (*Harness, error) {
	root := opts.Root
	if root == "" {
		var err error
		if root, err = FindRoot(); err != nil {
			return nil, err
		}
	}
	bundleDir, err := Ensure(root)
	if err != nil {
		return nil, err
	}
	return startHarness(ctx, root, bundleDir, opts)
}

func startHarness(ctx context.Context, root, bundleDir string, opts HarnessOptions) (*Harness, error) {
	timeout := opts.ReadyTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ready := newReadyLine()
	stderr := &lockedBuffer{}
	cmd := exec.Command("node", append([]string{filepath.Join(root, harnessScript)}, opts.args(bundleDir)...)...)
	cmd.Dir = filepath.Join(root, "cf-controller")
	cmd.Stdout = ready
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start the cf-controller harness: %w", err)
	}
	h := &Harness{cmd: cmd, stdin: stdin, done: make(chan struct{}), stderr: stderr}
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(h.done)
	}()

	select {
	case line := <-ready.line:
		var r struct {
			URL   string `json:"url"`
			Token string `json:"token"`
		}
		if err := json.Unmarshal([]byte(line), &r); err != nil || r.URL == "" {
			h.kill()
			return nil, fmt.Errorf("the cf-controller harness's ready line %q is not {url, token} (%v); stderr: %s", line, err, stderr)
		}
		h.url, h.token = r.URL, r.Token
		return h, nil
	case <-h.done:
		return nil, fmt.Errorf("the cf-controller harness exited before it was ready (%v): %s", waitErr, stderr)
	case <-time.After(timeout):
		h.kill()
		return nil, fmt.Errorf("the cf-controller harness was not ready in %s: %s", timeout, stderr)
	case <-ctx.Done():
		h.kill()
		return nil, fmt.Errorf("waiting for the cf-controller harness: %w", context.Cause(ctx))
	}
}

// URL is the harness's base URL, http://127.0.0.1:<port>.
func (h *Harness) URL() string { return h.url }

// Token is the owner token the harness accepts.
func (h *Harness) Token() string { return h.token }

// DoJSON sends a request to the harness's Durable Object doName (a
// /__test/* control endpoint, say) with reqBody, when not nil, as JSON, and
// decodes the 2xx reply into out, when not nil.
func (h *Harness) DoJSON(ctx context.Context, doName, method, path string, reqBody, out any) error {
	var body io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.url+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("X-Hotbed-Test-DO", doName)
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode %s %s: %w (body: %s)", method, path, err, raw)
		}
	}
	return nil
}

// closeTimeout is how long the harness gets to shut down on its own, once its
// stdin closes, before it is killed.
const closeTimeout = 5 * time.Second

// Close shuts the harness down: it closes the harness's stdin, which it
// shuts down on, and kills it if that takes more than closeTimeout. It is safe
// to call more than once.
func (h *Harness) Close() error {
	if err := h.stdin.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		h.kill()
		return err
	}
	select {
	case <-h.done:
	case <-time.After(closeTimeout):
		h.kill()
	}
	return nil
}

func (h *Harness) kill() {
	_ = h.cmd.Process.Kill()
	<-h.done
}

// readyLine is an io.Writer that keeps the first line written to it and
// discards the rest, so the child never blocks on a full stdout pipe.
type readyLine struct {
	line chan string
	buf  []byte
	sent bool
}

func newReadyLine() *readyLine { return &readyLine{line: make(chan string, 1)} }

func (r *readyLine) Write(p []byte) (int, error) {
	if r.sent {
		return len(p), nil
	}
	r.buf = append(r.buf, p...)
	if i := bytes.IndexByte(r.buf, '\n'); i >= 0 {
		r.line <- string(r.buf[:i])
		r.sent, r.buf = true, nil
	}
	return len(p), nil
}

// lockedBuffer is a bytes.Buffer safe to read while the child still writes it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(b.buf.String())
}
