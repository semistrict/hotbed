package t3bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ThreadState summarizes a local T3 Code thread's existence and busy state.
type ThreadState struct {
	Exists bool
	// Link is the thread's URL as T3 Code gives it
	// (https://app.t3.codes/threads/ENVIRONMENT/THREAD), which opens the
	// thread in the T3 Code app or on the web.
	Link                string
	Status              string
	ActiveRunID         string
	PendingRequestCount int
	BlockedOnThreadWait bool
	Archived            bool
}

// IsBusy reports whether the thread is currently executing or waiting on human
// input and should not be sent a wake message yet.
func (s ThreadState) IsBusy() bool {
	if s.PendingRequestCount > 0 || strings.TrimSpace(s.ActiveRunID) != "" || s.BlockedOnThreadWait {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(s.Status)) {
	case "running", "preparing", "starting", "queued", "waiting":
		return true
	default:
		return false
	}
}

// T3Client talks to the local T3 Code server.
type T3Client interface {
	ThreadStatus(ctx context.Context, threadID string) (ThreadState, error)
	SendMessage(ctx context.Context, threadID string, message string) error
}

// CLIClient invokes the local `t3` CLI binary.
type CLIClient struct {
	Binary  string
	BaseDir string
}

// ResolveT3Binary locates the `t3` CLI binary, checking explicit, $T3_BIN,
// PATH, and the default ~/.t3/bin/t3 install location (important for LaunchAgents
// where PATH does not include ~/.t3/bin).
func ResolveT3Binary(explicit string) (string, error) {
	if bin := strings.TrimSpace(explicit); bin != "" {
		if _, err := os.Stat(bin); err == nil {
			return bin, nil
		}
		if p, err := exec.LookPath(bin); err == nil {
			return p, nil
		}
		return "", fmt.Errorf("t3 binary %q not found", bin)
	}
	if envBin := strings.TrimSpace(os.Getenv("T3_BIN")); envBin != "" {
		if _, err := os.Stat(envBin); err == nil {
			return envBin, nil
		}
		if p, err := exec.LookPath(envBin); err == nil {
			return p, nil
		}
		return "", fmt.Errorf("T3_BIN %q not found", envBin)
	}
	if p, err := exec.LookPath("t3"); err == nil {
		return p, nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, cand := range []string{
			filepath.Join(home, ".t3", "bin", "t3"),
			filepath.Join(home, ".local", "bin", "t3"),
			"/opt/homebrew/bin/t3",
			"/usr/local/bin/t3",
		} {
			if st, err := os.Stat(cand); err == nil && !st.IsDir() {
				return cand, nil
			}
		}
	}
	return "", errors.New("could not find `t3` CLI on PATH or in ~/.t3/bin/t3 (pass --t3-bin or set T3_BIN)")
}

type t3ThreadShowOutput struct {
	Thread *struct {
		ThreadID            string  `json:"threadId"`
		Link                string  `json:"link"`
		Status              string  `json:"status"`
		ActiveRunID         *string `json:"activeRunId"`
		PendingRequestCount int     `json:"pendingRequestCount"`
		BlockedOnThreadWait bool    `json:"blockedOnThreadWait"`
		Archived            bool    `json:"archived"`
	} `json:"thread"`
	Error *string `json:"error"`
}

// ThreadStatus queries `t3 thread show <threadID> --messages 0 --json`.
// When the local T3 Code server is reachable but does not have threadID,
// ThreadStatus returns (ThreadState{Exists: false}, nil) so multi-machine setups
// skip threads hosted elsewhere.
func (c *CLIClient) ThreadStatus(ctx context.Context, threadID string) (ThreadState, error) {
	if strings.TrimSpace(c.Binary) == "" {
		return ThreadState{}, errors.New("t3 CLI binary path is empty")
	}
	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	args := []string{"thread", "show"}
	if dir := strings.TrimSpace(c.BaseDir); dir != "" {
		args = append(args, "--base-dir", dir)
	}
	args = append(args, threadID, "--messages", "0", "--json")

	cmd := exec.CommandContext(callCtx, c.Binary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		combined := strings.TrimSpace(stdout.String() + "\n" + stderr.String())
		if isThreadNotFoundOutput(combined) {
			return ThreadState{Exists: false}, nil
		}
		if combined != "" {
			return ThreadState{}, fmt.Errorf("t3 thread show %s: %w: %s", threadID, err, combined)
		}
		return ThreadState{}, fmt.Errorf("t3 thread show %s: %w", threadID, err)
	}

	var parsed t3ThreadShowOutput
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
		return ThreadState{}, fmt.Errorf("parse t3 thread show %s JSON: %w (stdout: %s)", threadID, err, strings.TrimSpace(stdout.String()))
	}
	if parsed.Thread == nil {
		if parsed.Error != nil && isThreadNotFoundOutput(*parsed.Error) {
			return ThreadState{Exists: false}, nil
		}
		return ThreadState{}, fmt.Errorf("t3 thread show %s returned no thread object", threadID)
	}
	activeRun := ""
	if parsed.Thread.ActiveRunID != nil {
		activeRun = *parsed.Thread.ActiveRunID
	}
	return ThreadState{
		Exists:              true,
		Link:                parsed.Thread.Link,
		Status:              parsed.Thread.Status,
		ActiveRunID:         activeRun,
		PendingRequestCount: parsed.Thread.PendingRequestCount,
		BlockedOnThreadWait: parsed.Thread.BlockedOnThreadWait,
		Archived:            parsed.Thread.Archived,
	}, nil
}

// SendMessage delivers message to threadID via `t3 thread send <threadID> - --json`
// with the message piped on stdin.
func (c *CLIClient) SendMessage(ctx context.Context, threadID string, message string) error {
	if strings.TrimSpace(c.Binary) == "" {
		return errors.New("t3 CLI binary path is empty")
	}
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	args := []string{"thread", "send"}
	if dir := strings.TrimSpace(c.BaseDir); dir != "" {
		args = append(args, "--base-dir", dir)
	}
	args = append(args, threadID, "-", "--json")

	cmd := exec.CommandContext(callCtx, c.Binary, args...)
	cmd.Stdin = strings.NewReader(message)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		combined := strings.TrimSpace(stdout.String() + "\n" + stderr.String())
		if combined != "" {
			return fmt.Errorf("t3 thread send %s: %w: %s", threadID, err, combined)
		}
		return fmt.Errorf("t3 thread send %s: %w", threadID, err)
	}
	return nil
}

func isThreadNotFoundOutput(out string) bool {
	lower := strings.ToLower(out)
	if strings.Contains(lower, "no running t3 code server") ||
		strings.Contains(lower, "connection refused") ||
		strings.Contains(lower, "econnrefused") {
		return false
	}
	return strings.Contains(lower, "is no longer available") ||
		strings.Contains(lower, "thread not found") ||
		strings.Contains(lower, "no such thread")
}
