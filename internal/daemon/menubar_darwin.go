//go:build darwin

package daemon

import (
	"bufio"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

//go:embed menubar_darwin.swift
var menubarSwiftSource string

func ensureMenubarBinary(ctx context.Context, baseDir string) (string, error) {
	sum := sha256.Sum256([]byte(menubarSwiftSource))
	hash := hex.EncodeToString(sum[:8])
	cacheDir := filepath.Join(baseDir, "cache")
	binPath := filepath.Join(cacheDir, "hotbed-menubar-"+hash)
	if st, err := os.Stat(binPath); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
		return binPath, nil
	}

	swiftc, err := exec.LookPath("swiftc")
	if err != nil {
		if _, serr := os.Stat("/usr/bin/swiftc"); serr == nil {
			swiftc = "/usr/bin/swiftc"
		} else {
			return "", fmt.Errorf("swiftc not found (install Xcode Command Line Tools for the macOS menubar app)")
		}
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", err
	}

	srcFile, err := os.CreateTemp(cacheDir, "hotbed-menubar-*.swift")
	if err != nil {
		return "", err
	}
	srcPath := srcFile.Name()
	defer os.Remove(srcPath)
	if _, err := io.WriteString(srcFile, menubarSwiftSource); err != nil {
		srcFile.Close()
		return "", err
	}
	if err := srcFile.Close(); err != nil {
		return "", err
	}

	tmpBin := binPath + ".tmp"
	defer os.Remove(tmpBin)

	buildCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(buildCtx, swiftc, "-O", "-framework", "AppKit", "-o", tmpBin, srcPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("compile macOS menubar helper: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if err := os.Rename(tmpBin, binPath); err != nil {
		return "", err
	}
	// Prune older cached menubar binaries.
	if entries, err := os.ReadDir(cacheDir); err == nil {
		base := filepath.Base(binPath)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "hotbed-menubar-") && e.Name() != base {
				_ = os.Remove(filepath.Join(cacheDir, e.Name()))
			}
		}
	}
	return binPath, nil
}

func (d *Daemon) runMenubar(ctx context.Context) {
	binPath, err := ensureMenubarBinary(ctx, d.baseDir)
	if err != nil {
		d.log.Info("daemon: macOS menubar disabled", "reason", err.Error())
		return
	}

	backoff := 2 * time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := d.runMenubarOnce(ctx, binPath)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > 15*time.Second {
			backoff = 2 * time.Second
		}
		d.log.Warn("daemon: macOS menubar exited; restarting", "backoff", backoff, "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
			backoff = min(backoff*2, 30*time.Second)
		}
	}
}

func (d *Daemon) runMenubarOnce(ctx context.Context, binPath string) error {
	cmd := exec.CommandContext(ctx, binPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return err
	}

	subCh, initial, unsub := d.subscribe()
	defer unsub()

	writeSnap := func(s Snapshot) error {
		b, err := json.Marshal(s)
		if err != nil {
			return err
		}
		b = append(b, '\n')
		_, err = stdin.Write(b)
		return err
	}

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		defer stdin.Close()
		if err := writeSnap(initial); err != nil {
			return
		}
		for {
			select {
			case <-ctx.Done():
				return
			case snap := <-subCh:
				if err := writeSnap(snap); err != nil {
					return
				}
			}
		}
	}()

	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			var msg struct {
				Action string `json:"action"`
			}
			if err := json.Unmarshal(sc.Bytes(), &msg); err == nil && msg.Action != "" {
				d.handleMenubarAction(ctx, msg.Action)
			}
		}
	}()

	err = cmd.Wait()
	<-writerDone
	return err
}
