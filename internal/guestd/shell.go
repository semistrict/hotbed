package guestd

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/semistrict/hotbed/internal/gitops"
	"github.com/semistrict/hotbed/internal/pty"
	"github.com/semistrict/hotbed/internal/unit"
)

// Interactive vsock shell, TCP port proxy and git-auth bridge served by
// hotbed-guestd inside the VM.

// ServeShell accepts interactive shell connections on ln (ShellPort) until ctx ends.
func (s *Server) ServeShell(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handleShellConn(ctx, conn)
	}
}

func (s *Server) handleShellConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	var req ShellOpenRequest
	if err := readJSONFrame(conn, frameOpen, &req); err != nil {
		_ = writeFrame(conn, frameError, []byte(err.Error()))
		return
	}
	argv := req.Argv
	if len(argv) == 0 {
		argv = []string{"/bin/bash", "-l"}
	}
	dir := s.resolveWorkDir(req.Dir)

	var writeMu sync.Mutex
	send := func(typ byte, payload []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return writeFrame(conn, typ, payload)
	}

	runtimeDir := s.ensureRuntimeDir(req.UID, req.GID)
	env := append([]string(nil), req.Env...)
	if req.Term != "" {
		env = append(env, "TERM="+req.Term)
	}
	env = withDefaultRuntimeEnv(env, runtimeDir)
	cmd, err := commandWithEnv(argv, env)
	if err != nil {
		_ = send(frameError, []byte("start shell: "+err.Error()))
		return
	}
	cmd.Dir = dir
	attr := &syscall.SysProcAttr{}
	if (req.UID > 0 || req.GID > 0) && os.Geteuid() == 0 {
		attr.Credential = &syscall.Credential{
			Uid: uint32(req.UID),
			Gid: uint32(req.GID),
		}
	}

	if req.PTY {
		master, slave, _, err := pty.Open()
		if err != nil {
			_ = send(frameError, []byte("open pty: "+err.Error()))
			return
		}
		cols, rows := req.Cols, req.Rows
		if cols == 0 {
			cols = 80
		}
		if rows == 0 {
			rows = 24
		}
		_ = pty.Resize(master, cols, rows)

		attr.Setsid = true
		attr.Setctty = true
		cmd.SysProcAttr = attr
		cmd.Stdin = slave
		cmd.Stdout = slave
		cmd.Stderr = slave
		if err := cmd.Start(); err != nil {
			_ = slave.Close()
			_ = master.Close()
			_ = send(frameError, []byte("start shell: "+err.Error()))
			return
		}
		_ = slave.Close()
		group := unit.NewProcGroup(cmd)

		// Pump PTY master -> host.
		outDone := make(chan struct{})
		go func() {
			defer close(outDone)
			buf := make([]byte, 16<<10)
			for {
				n, err := master.Read(buf)
				if n > 0 {
					if send(frameData, buf[:n]) != nil {
						return
					}
				}
				if err != nil {
					return
				}
			}
		}()

		// Pump host -> PTY master & handle resize frames.
		go func() {
			for {
				typ, payload, err := readFrame(conn)
				if err != nil {
					_ = group.Signal(syscall.SIGHUP)
					_ = master.Close()
					return
				}
				switch typ {
				case frameData:
					if len(payload) > 0 {
						_, _ = master.Write(payload)
					}
				case frameResize:
					if len(payload) == 4 {
						c := binary.BigEndian.Uint16(payload[0:2])
						rw := binary.BigEndian.Uint16(payload[2:4])
						_ = pty.Resize(master, c, rw)
					}
				case frameCloseStdin:
					// PTY has no half-close; send EOT if needed or ignore.
				}
			}
		}()

		waitErr := group.Wait(false)
		// Drain buffered PTY output before closing master: once the child
		// closes the slave fd, master.Read returns the remaining bytes and
		// then EIO/EOF. Bound the wait in case a grandchild kept the slave open.
		select {
		case <-outDone:
		case <-time.After(250 * time.Millisecond):
			_ = master.Close()
			<-outDone
		}
		_ = master.Close()
		var code [4]byte
		binary.BigEndian.PutUint32(code[:], uint32(int32(exitCodeOf(waitErr))))
		_ = send(frameExit, code[:])
		return
	}

	// Piped (non-PTY) shell.
	attr.Setpgid = true
	cmd.SysProcAttr = attr
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		_ = send(frameError, []byte(err.Error()))
		return
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		_ = send(frameError, []byte(err.Error()))
		return
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		_ = send(frameError, []byte(err.Error()))
		return
	}
	if err := cmd.Start(); err != nil {
		_ = send(frameError, []byte("start shell: "+err.Error()))
		return
	}
	group := unit.NewProcGroup(cmd)

	var outWG sync.WaitGroup
	pump := func(r io.Reader, typ byte) {
		defer outWG.Done()
		buf := make([]byte, 16<<10)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				if send(typ, buf[:n]) != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	outWG.Add(2)
	go pump(stdoutPipe, frameData)
	go pump(stderrPipe, frameStderr)

	go func() {
		for {
			typ, payload, err := readFrame(conn)
			if err != nil {
				_ = stdinPipe.Close()
				_ = group.Signal(syscall.SIGTERM)
				return
			}
			switch typ {
			case frameData:
				if len(payload) > 0 {
					_, _ = stdinPipe.Write(payload)
				}
			case frameCloseStdin:
				_ = stdinPipe.Close()
			}
		}
	}()

	outWG.Wait()
	waitErr := group.Wait(false)
	var code [4]byte
	binary.BigEndian.PutUint32(code[:], uint32(int32(exitCodeOf(waitErr))))
	_ = send(frameExit, code[:])
}

// ServeTCPProxy accepts host-initiated dev-server proxy connections on ln
// (TCPProxyPort), dials 127.0.0.1:<port> inside the guest, and bridges bytes.
func (s *Server) ServeTCPProxy(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go handleTCPProxyConn(conn)
	}
}

func handleTCPProxyConn(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	line, err := br.ReadBytes('\n')
	if err != nil {
		return
	}
	var req TCPDialRequest
	if err := json.Unmarshal(line, &req); err != nil || req.Port == 0 {
		_ = json.NewEncoder(conn).Encode(TCPDialResponse{OK: false, Err: "invalid port"})
		return
	}
	target, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", req.Port), 5*time.Second)
	if err != nil {
		_ = json.NewEncoder(conn).Encode(TCPDialResponse{OK: false, Err: err.Error()})
		return
	}
	defer target.Close()
	if err := json.NewEncoder(conn).Encode(TCPDialResponse{OK: true}); err != nil {
		return
	}
	BridgeConns(&bufferedConn{Conn: conn, r: br}, target)
}

// StartGitAuthBridge exposes a local Unix socket at sockPath inside the guest
// (e.g. /run/hotbed/git-auth/git-cred.sock) and forwards every connection to
// dialHost (vsock CID 2, GitAuthPort on Firecracker).
func StartGitAuthBridge(ctx context.Context, sockPath string, dialHost func() (net.Conn, error)) error {
	dir := filepath.Dir(sockPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "git-credential-helper"), []byte(gitops.GitCredentialHelperScript(sockPath)), 0o755); err != nil {
		return fmt.Errorf("write git credential helper: %w", err)
	}
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return err
	}
	_ = os.Chmod(sockPath, 0o666)
	go func() {
		<-ctx.Done()
		_ = ln.Close()
		_ = os.Remove(sockPath)
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go func(local net.Conn) {
			defer local.Close()
			remote, err := dialHost()
			if err != nil {
				return
			}
			defer remote.Close()
			BridgeConns(local, remote)
		}(c)
	}
}

// BridgeConns copies bytes both ways between a and b until each direction has
// ended, then returns. When a direction ends, the connection it was writing to
// is half-closed (CloseWrite) if it can be, so the peer sees EOF while its own
// replies still flow back. It closes neither connection.
func BridgeConns(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
}
