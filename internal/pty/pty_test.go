//go:build linux || darwin

package pty

import (
	"io"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

func TestOpenAndResize(t *testing.T) {
	master, slave, path, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	if path == "" {
		t.Fatal("empty slave path")
	}
	if err := Resize(master, 117, 39); err != nil {
		slave.Close()
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", "stty size; tty")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		slave.Close()
		t.Fatal(err)
	}
	slave.Close()
	out, _ := io.ReadAll(master)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("cmd wait: %v (out=%q)", err, out)
	}
	s := strings.ReplaceAll(string(out), "\r\n", "\n")
	if !strings.Contains(s, "39 117") {
		t.Errorf("stty size output = %q, want 39 117", s)
	}
	if !strings.Contains(s, path) {
		t.Errorf("tty output = %q, want %q", s, path)
	}
}
