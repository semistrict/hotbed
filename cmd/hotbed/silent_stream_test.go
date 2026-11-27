package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/worker"
)

// muteWriter is a response that, once muted, takes what is written to it
// and passes none of it on.
type muteWriter struct {
	http.ResponseWriter
	mu    sync.Mutex
	muted bool
}

func (m *muteWriter) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.muted {
		return len(p), nil
	}
	return m.ResponseWriter.Write(p)
}

func (m *muteWriter) Flush() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.muted {
		m.ResponseWriter.(http.Flusher).Flush()
	}
}

func (m *muteWriter) mute() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.muted = true
}

func (m *muteWriter) isMuted() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.muted
}

// silencer stands where Cloudflare's edge does, between the CLI and the
// controller: silence kills every FollowLog stream open through it the way
// a controller deploy does, ending its controller's end (and so the
// worker's) while holding the CLI's end open, saying nothing, until the
// CLI gives up on it.
type silencer struct {
	mu      sync.Mutex
	live    []func()
	follows int
}

func (s *silencer) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != hotbedv1connect.WorkspaceServiceFollowLogProcedure {
			next.ServeHTTP(w, r)
			return
		}
		upstream, cut := context.WithCancel(r.Context())
		defer cut()
		mw := &muteWriter{ResponseWriter: w}
		s.mu.Lock()
		s.follows++
		s.live = append(s.live, func() { mw.mute(); cut() })
		s.mu.Unlock()
		serve(next, mw, r.WithContext(upstream))
		if mw.isMuted() {
			<-r.Context().Done()
		}
	})
}

// serve is next serving mw, but a muted stream's abort (the reverse proxy
// aborts a response whose upstream it lost) is not passed on: an abort
// would reset the CLI's stream, which is not the silence being tested.
func serve(next http.Handler, mw *muteWriter, r *http.Request) {
	defer func() {
		if p := recover(); p != nil && (p != http.ErrAbortHandler || !mw.isMuted()) {
			panic(p)
		}
	}()
	next.ServeHTTP(mw, r)
}

func (s *silencer) silence() {
	s.mu.Lock()
	live := s.live
	s.live = nil
	s.mu.Unlock()
	for _, kill := range live {
		kill()
	}
}

func (s *silencer) followed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.follows
}

// An attached `hotbed run` whose output stream dies without a word, as in
// a controller deploy, follows its command again on a new stream: it
// prints the command's output and exits with its status, whether the
// command ends while the stream is dead (issue #21) or goes on past it
// (issue #20, where the worker then stopped it as orphaned), rather than
// wait on the dead stream forever.
func TestAttachRunOutlivesAStreamThatDiesSilently(t *testing.T) {
	for _, tc := range []struct {
		name string
		// endsWhileDead: the command ends before the run notices its stream died.
		endsWhileDead bool
	}{
		{"command ends while the stream is dead", true},
		{"command goes on past the dead stream", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newCluster(t, func(cfg *worker.EngineConfig) { cfg.StreamKeepalive = 100 * time.Millisecond })
			c.cli.silence = time.Second
			c.addRepo(upstream(t))
			c.ok(nil, "session", "start", "hushed-elk")
			c.workspace("hushed-elk", nil)

			s := &silencer{}
			next := *c.handler.Load()
			wrapped := s.wrap(next)
			c.handler.Store(&wrapped)

			gate := filepath.Join(t.TempDir(), "release")
			t.Cleanup(func() { _ = os.WriteFile(gate, nil, 0o644) })
			script := fmt.Sprintf(`echo before; while [ ! -e %q ]; do sleep 0.05; done; echo after; exit 7`, gate)

			var stdout, stderr syncBuffer
			d := c.cli
			d.stdin = strings.NewReader("")
			d.stdout, d.stderr = &stdout, &stderr
			done := make(chan int, 1)
			go func() {
				done <- run(t.Context(), []string{"run", "-s", "hushed-elk", "-r", "app", "--", "sh", "-c", script}, d)
			}()
			c.eventually("the run's first output", func() bool {
				return strings.Contains(stdout.String(), "before\n")
			})

			followed := s.followed()
			s.silence()
			if tc.endsWhileDead {
				if err := os.WriteFile(gate, nil, 0o644); err != nil {
					t.Fatal(err)
				}
			} else {
				c.eventually("the run to follow on a new stream", func() bool { return s.followed() > followed })
				if err := os.WriteFile(gate, nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}

			select {
			case code := <-done:
				if code != 7 {
					t.Fatalf("hotbed run exited %d, want 7 (stdout=%q stderr=%q)", code, stdout.String(), stderr.String())
				}
			case <-time.After(20 * time.Second):
				t.Fatalf("hotbed run hung on its dead stream (stdout=%q stderr=%q)", stdout.String(), stderr.String())
			}
			if got := stdout.String(); got != "before\nafter\n" {
				t.Errorf("stdout = %q, want %q", got, "before\nafter\n")
			}
		})
	}
}
