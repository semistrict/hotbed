package main

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/semistrict/hotbed/internal/api"
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

// bindSessionForContext records sessionID in ~/.hotbed/sessions.json under
// T3CODE_THREAD_ID when set, otherwise under the current git worktree root.
func bindSessionForContext(ctx context.Context, d deps, sessionID string) {
	path := sessionsFilePath(d)
	if path == "" || sessionID == "" {
		return
	}
	m, err := loadSessionMap(path)
	if err != nil {
		return
	}
	changed := false
	if tid := strings.TrimSpace(os.Getenv("T3CODE_THREAD_ID")); tid != "" {
		m.Threads[tid] = sessionID
		changed = true
	} else if cwd, err := os.Getwd(); err == nil {
		if root, err := gitCheckoutRoot(ctx, cwd); err == nil && root != "" {
			m.Worktrees[canonicalPath(root)] = sessionID
			changed = true
		}
	}
	if changed {
		_ = saveSessionMap(path, m)
	}
}

// unbindSession removes any thread or worktree mapping pointing to sessionID.
func unbindSession(d deps, sessionID string) {
	path := sessionsFilePath(d)
	if path == "" || sessionID == "" {
		return
	}
	m, err := loadSessionMap(path)
	if err != nil {
		return
	}
	changed := false
	for k, v := range m.Threads {
		if v == sessionID {
			delete(m.Threads, k)
			changed = true
		}
	}
	for k, v := range m.Worktrees {
		if v == sessionID {
			delete(m.Worktrees, k)
			changed = true
		}
	}
	if changed {
		_ = saveSessionMap(path, m)
	}
}

// resolve returns the active session binding in priority order:
//  1. -s/--session (or HOTBED_SESSION via Kong env)
//  2. T3CODE_THREAD_ID mapping in ~/.hotbed/sessions.json
//  3. current git worktree mapping in ~/.hotbed/sessions.json
func (f sessionFlag) resolveBinding(ctx context.Context, d deps) (sessionBinding, error) {
	if s := strings.TrimSpace(f.Session); s != "" {
		if strings.TrimSpace(os.Getenv("HOTBED_SESSION")) == s {
			return sessionBinding{ID: s, Source: "env", Key: "HOTBED_SESSION"}, nil
		}
		return sessionBinding{ID: s, Source: "flag"}, nil
	}
	m, err := loadSessionMap(sessionsFilePath(d))
	if err == nil {
		if tid := strings.TrimSpace(os.Getenv("T3CODE_THREAD_ID")); tid != "" {
			if s := m.Threads[tid]; s != "" {
				return sessionBinding{ID: s, Source: "thread", Key: tid}, nil
			}
		}
		if cwd, err := os.Getwd(); err == nil {
			if root, err := gitCheckoutRoot(ctx, cwd); err == nil && root != "" {
				canon := canonicalPath(root)
				if s := m.Worktrees[canon]; s != "" {
					return sessionBinding{ID: s, Source: "worktree", Key: canon}, nil
				}
			}
		}
	}
	return sessionBinding{}, api.Errorf(api.CodeInvalidArgs, "no session specified and none bound to this thread or worktree").
		WithSuggestion("run `hotbed session start`, set HOTBED_SESSION, or pass -s SESSION")
}

func (f sessionFlag) resolve(ctx context.Context, d deps) (string, error) {
	b, err := f.resolveBinding(ctx, d)
	if err != nil {
		return "", err
	}
	return b.ID, nil
}
