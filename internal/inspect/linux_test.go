//go:build linux

package inspect

import (
	"context"
	"net"
	"os"
	"testing"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

func TestLinuxInspectSelfProcess(t *testing.T) {
	// Open a real TCP listener so /proc/net/tcp socket resolution is exercised end-to-end.
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp4: %v", err)
	}
	defer ln.Close()
	wantAddr := ln.Addr().String()

	res, err := (Linux{}).Inspect(context.Background(), Target{
		CommandID: "self-test",
		UnitName:  "hotbed-self-test",
		MainPID:   os.Getpid(),
	}, &pb.InspectCommandRequest{Id: "self-test"})
	if err != nil {
		t.Fatalf("Linux.Inspect: %v", err)
	}
	if len(res.Procs) != 1 || int(res.Procs[0].Pid) != os.Getpid() {
		t.Fatalf("expected self PID %d in procs, got %+v", os.Getpid(), res.Procs)
	}
	if res.Procs[0].RssBytes <= 0 {
		t.Fatalf("expected positive RSS, got %d", res.Procs[0].RssBytes)
	}
	foundSock := false
	for _, f := range res.Files {
		if f.Kind == "socket" && f.SocketState == "LISTEN" && f.LocalAddr == wantAddr {
			foundSock = true
			break
		}
	}
	if !foundSock {
		t.Fatalf("expected listening TCP socket %s in open files, got %d files", wantAddr, len(res.Files))
	}
	if len(res.Maps) == 0 || res.Maps[0].RssBytes <= 0 {
		t.Fatalf("expected memory map for self process, got %+v", res.Maps)
	}
	if len(res.Limits) == 0 || len(res.Limits[0].Items) == 0 {
		t.Fatalf("expected rlimits for self process, got %+v", res.Limits)
	}
	if len(res.AvailableTools) == 0 {
		t.Fatalf("expected tool capabilities list")
	}
}
