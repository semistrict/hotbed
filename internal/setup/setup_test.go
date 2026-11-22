package setup

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestQMPClient(t *testing.T) {
	// Keep socket path short for macOS 104-byte Unix socket limit.
	tmpDir, err := os.MkdirTemp("/tmp", "hb-qmp-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpDir) })
	sock := filepath.Join(tmpDir, "q.sock")

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	gotPowerdown := make(chan bool, 1)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = c.Write([]byte(`{"QMP":{"version":{"qemu":{"major":11}}}}` + "\n"))
				sc := bufio.NewScanner(c)
				for sc.Scan() {
					var req struct {
						Execute string `json:"execute"`
					}
					if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
						return
					}
					switch req.Execute {
					case "qmp_capabilities":
						_, _ = c.Write([]byte(`{"return":{}}` + "\n"))
					case "query-status":
						// Send an async event first to ensure the client ignores events.
						_, _ = c.Write([]byte(`{"event":"RESUME"}` + "\n"))
						_, _ = c.Write([]byte(`{"return":{"status":"running"}}` + "\n"))
					case "system_powerdown":
						gotPowerdown <- true
						_, _ = c.Write([]byte(`{"return":{}}` + "\n"))
					}
				}
			}(conn)
		}
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	st, err := QMPQueryStatus(ctx, sock)
	if err != nil {
		t.Fatalf("QMPQueryStatus: %v", err)
	}
	if st != "running" {
		t.Fatalf("status = %q, want running", st)
	}
	if err := QMPPowerdown(ctx, sock); err != nil {
		t.Fatalf("QMPPowerdown: %v", err)
	}
	select {
	case <-gotPowerdown:
	case <-time.After(time.Second):
		t.Fatal("mock QMP server did not receive system_powerdown")
	}
}
