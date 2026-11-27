package main

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/gitops"
	"github.com/semistrict/hotbed/internal/setup"
)

// sessionFlag is the flag every session-scoped command takes. Session is never
// a positional argument: pass -s/--session, set HOTBED_SESSION, or rely on the
// session bound to T3CODE_THREAD_ID / the current git worktree in ~/.hotbed/sessions.json.
type sessionFlag struct {
	Session string `short:"s" env:"HOTBED_SESSION" help:"The session's name (default: HOTBED_SESSION, or the session started for T3CODE_THREAD_ID / this git worktree)."`
}

// repoFlag is the flag every repo-scoped command takes. Repo is never a
// positional argument: pass -r/--repo, set HOTBED_REPO, or let hotbed infer
// it from the current git checkout.
type repoFlag struct {
	Repo string `short:"r" env:"HOTBED_REPO" help:"The repo's name (default: HOTBED_REPO, or inferred from the current git checkout)."`
}

// sessionMap is persisted in ~/.hotbed/sessions.json.
type sessionMap struct {
	Threads   map[string]string `json:"threads,omitempty"`
	Worktrees map[string]string `json:"worktrees,omitempty"`
}

// sessionBinding describes where the resolved session name came from.
type sessionBinding struct {
	ID     string `json:"id"`
	Source string `json:"source"` // "flag", "env", "thread", "worktree"
	Key    string `json:"key,omitempty"`
}

func defaultSessionsFile() string {
	dir, err := setup.DefaultDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "sessions.json")
}

func sessionsFilePath(d deps) string {
	if d.sessionsFile != "" {
		return d.sessionsFile
	}
	if d.cache != "" {
		return filepath.Join(d.cache, "sessions.json")
	}
	return defaultSessionsFile()
}

func loadSessionMap(path string) (sessionMap, error) {
	m := sessionMap{
		Threads:   map[string]string{},
		Worktrees: map[string]string{},
	}
	if path == "" {
		return m, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, err
	}
	if m.Threads == nil {
		m.Threads = map[string]string{}
	}
	if m.Worktrees == nil {
		m.Worktrees = map[string]string{}
	}
	return m, nil
}

func saveSessionMap(path string, m sessionMap) error {
	if path == "" {
		return nil
	}
	// Prune worktree entries whose directories no longer exist.
	for wt := range m.Worktrees {
		if _, err := os.Stat(wt); errors.Is(err, fs.ErrNotExist) {
			delete(m.Worktrees, wt)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), "sessions.json.*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// bindPlace is where session start binds a session: this T3 thread when
// T3CODE_THREAD_ID is set, else the git worktree around the working directory.
type bindPlace struct {
	Source string // "thread" or "worktree"
	Key    string
}

// String names the place for a person: "T3 thread X" or "git worktree X".
func (p bindPlace) String() string {
	if p.Source == "thread" {
		return "T3 thread " + p.Key
	}
	return "git worktree " + p.Key
}

// startPlace returns where session start binds; ok is false outside a T3
// thread and a git checkout, where nothing binds.
func (h here) startPlace(ctx context.Context) (p bindPlace, ok bool) {
	if tid := h.t3codeThreadID(); tid != "" {
		return bindPlace{Source: "thread", Key: tid}, true
	}
	return h.worktreePlace(ctx)
}

// worktreePlace is the git worktree around the working directory.
func (h here) worktreePlace(ctx context.Context) (p bindPlace, ok bool) {
	root, err := h.checkoutRoot(ctx)
	if err != nil || root == "" {
		return bindPlace{}, false
	}
	return bindPlace{Source: "worktree", Key: gitops.CanonicalPath(root)}, true
}

// bound returns the session bound at p, or "".
func (m sessionMap) bound(p bindPlace) string {
	if p.Source == "thread" {
		return m.Threads[p.Key]
	}
	return m.Worktrees[p.Key]
}

// bind binds sessionID at p.
func (m sessionMap) bind(p bindPlace, sessionID string) {
	if p.Source == "thread" {
		m.Threads[p.Key] = sessionID
	} else {
		m.Worktrees[p.Key] = sessionID
	}
}

// unbind removes every binding of sessionID.
func (m sessionMap) unbind(sessionID string) {
	for k, v := range m.Threads {
		if v == sessionID {
			delete(m.Threads, k)
		}
	}
	for k, v := range m.Worktrees {
		if v == sessionID {
			delete(m.Worktrees, k)
		}
	}
}

// updateSessionMapIf applies f to the sessions file at path under an exclusive
// file lock and writes the file back only when f returns true.
func updateSessionMapIf(path string, f func(sessionMap) bool) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	lf, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN) }()

	m, err := loadSessionMap(path)
	if err != nil {
		return err
	}
	if !f(m) {
		return nil
	}
	return saveSessionMap(path, m)
}

// updateSessionMap applies f to the sessions file at path under an exclusive
// file lock so concurrent session start/end/mcp updates do not clobber each
// other.
func updateSessionMap(path string, f func(sessionMap)) error {
	return updateSessionMapIf(path, func(m sessionMap) bool {
		f(m)
		return true
	})
}

// unbindSession removes every binding of sessionID from the sessions file.
func unbindSession(d deps, sessionID string) error {
	return updateSessionMap(sessionsFilePath(d), func(m sessionMap) { m.unbind(sessionID) })
}

// lookupBinding returns the active session binding in priority order, found
// false when there is none:
//  1. -s/--session (or HOTBED_SESSION via Kong env)
//  2. T3CODE_THREAD_ID mapping in ~/.hotbed/sessions.json
//  3. current git worktree mapping in ~/.hotbed/sessions.json
func (f sessionFlag) lookupBinding(ctx context.Context, d deps) (b sessionBinding, found bool, err error) {
	if s := strings.TrimSpace(f.Session); s != "" {
		if d.env("HOTBED_SESSION") == s {
			return sessionBinding{ID: s, Source: "env", Key: "HOTBED_SESSION"}, true, nil
		}
		return sessionBinding{ID: s, Source: "flag"}, true, nil
	}
	m, err := loadSessionMap(sessionsFilePath(d))
	if err != nil {
		return sessionBinding{}, false, err
	}
	// A thread with no session of its own uses its worktree's.
	var places []bindPlace
	if tid := d.t3codeThreadID(); tid != "" {
		places = append(places, bindPlace{Source: "thread", Key: tid})
	}
	if p, ok := d.worktreePlace(ctx); ok {
		places = append(places, p)
	}
	for _, p := range places {
		if s := m.bound(p); s != "" {
			return sessionBinding{ID: s, Source: p.Source, Key: p.Key}, true, nil
		}
	}
	return sessionBinding{}, false, nil
}

// resolveBinding is lookupBinding, failing when there is no session.
func (f sessionFlag) resolveBinding(ctx context.Context, d deps) (sessionBinding, error) {
	b, found, err := f.lookupBinding(ctx, d)
	if err != nil {
		return sessionBinding{}, err
	}
	if !found {
		return sessionBinding{}, api.Errorf(api.CodeInvalidArgs, "no session specified and none bound to this thread or worktree").
			WithSuggestion("run `hotbed session start`, set HOTBED_SESSION, or pass -s SESSION")
	}
	return b, nil
}

// optional is the active session's ID, empty when there is none: for a
// call that needs none but goes by one where it can.
func (f sessionFlag) optional(ctx context.Context, d deps) (string, error) {
	b, _, err := f.lookupBinding(ctx, d)
	return b.ID, err
}

func (f sessionFlag) resolve(ctx context.Context, d deps) (string, error) {
	b, err := f.resolveBinding(ctx, d)
	if err != nil {
		return "", err
	}
	return b.ID, nil
}

// named is the session of a change the user makes, which an agent passing
// it on names with -s or HOTBED_SESSION; none otherwise. A session merely
// bound to this thread or worktree is not used: it may long have ended, and
// the change is the user's.
func (f sessionFlag) named() string {
	return strings.TrimSpace(f.Session)
}
