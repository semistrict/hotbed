// Command hotbed-guestd runs inside each workspace Firecracker microVM (either
// as PID 1 or a service), exposing control, interactive shell, TCP proxy, and
// host-bound git-auth bridging over virtio-vsock.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"golang.org/x/sync/errgroup"

	"github.com/semistrict/hotbed/internal/guestd"
)

func main() {
	var (
		workDir     = flag.String("work-dir", "/work", "workspace root directory")
		stateDir    = flag.String("state-dir", "/run/hotbed-guestd", "runtime state directory for logs and sync bundles")
		listenDir   = flag.String("listen-dir", "", "if set, listen on Unix sockets in this directory instead of AF_VSOCK (for tests)")
		gitAuthSock = flag.String("git-auth-sock", "/run/hotbed/git-auth/git-cred.sock", "guest Unix socket path bridged to host git-auth over vsock (empty disables)")
		pid1        = flag.Bool("pid1", false, "mount Linux API filesystems and supervise child when running as PID 1")
		noPID1      = flag.Bool("no-pid1", false, "run server directly without PID 1 supervisor wrapper")
	)
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if !*noPID1 && (*pid1 || os.Getpid() == 1) {
		if runPID1Supervisor(log) {
			return
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv, err := guestd.NewServer(guestd.ServerConfig{
		WorkDir:  *workDir,
		StateDir: *stateDir,
		Logger:   log,
	})
	if err != nil {
		log.Error("init guestd server failed", "err", err)
		os.Exit(1)
	}

	listenPort := func(port uint32) (net.Listener, error) {
		if *listenDir != "" {
			if err := os.MkdirAll(*listenDir, 0o755); err != nil {
				return nil, err
			}
			p := filepath.Join(*listenDir, fmt.Sprintf("port-%d.sock", port))
			_ = os.Remove(p)
			return net.Listen("unix", p)
		}
		return guestd.ListenVsock(port)
	}

	ctrlLn, err := listenPort(guestd.ControlPort)
	if err != nil {
		log.Error("listen control port failed", "port", guestd.ControlPort, "err", err)
		os.Exit(1)
	}
	shellLn, err := listenPort(guestd.ShellPort)
	if err != nil {
		log.Error("listen shell port failed", "port", guestd.ShellPort, "err", err)
		os.Exit(1)
	}
	tcpLn, err := listenPort(guestd.TCPProxyPort)
	if err != nil {
		log.Error("listen tcp proxy port failed", "port", guestd.TCPProxyPort, "err", err)
		os.Exit(1)
	}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return srv.ServeControl(gctx, ctrlLn) })
	g.Go(func() error { return srv.ServeShell(gctx, shellLn) })
	g.Go(func() error { return srv.ServeTCPProxy(gctx, tcpLn) })

	if *gitAuthSock != "" && *listenDir == "" {
		g.Go(func() error {
			return guestd.StartGitAuthBridge(gctx, *gitAuthSock, func() (net.Conn, error) {
				return guestd.DialVsock(guestd.HostCID, guestd.GitAuthPort)
			})
		})
	}

	log.Info("hotbed-guestd ready",
		"workDir", *workDir,
		"stateDir", *stateDir,
		"listenDir", *listenDir,
		"pid1", os.Getpid() == 1,
	)

	if err := g.Wait(); err != nil {
		log.Error("hotbed-guestd exited with error", "err", err)
		os.Exit(1)
	}
}
