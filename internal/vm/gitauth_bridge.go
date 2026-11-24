package vm

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"strconv"
	"sync"

	"github.com/semistrict/hotbed/internal/guestd"
)

// gitAuthBridge listens on Firecracker's guest-to-host vsock Unix socket
// (<vsockUDS>_5002) and forwards every connection from hotbed-guestd inside the
// VM to the host worker's gitAuthServer Unix socket (git-cred.sock).
// No GitHub tokens ever enter the VM filesystem.
type gitAuthBridge struct {
	sockPath   string
	targetSock string
	ln         net.Listener
	closeOnce  sync.Once
}

func startGitAuthBridge(vsockUDS, targetHostSock, vmUser string) (*gitAuthBridge, error) {
	sockPath := fmt.Sprintf("%s_%d", vsockUDS, guestd.GitAuthPort)
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("listen git-auth vsock bridge %s: %w", sockPath, err)
	}
	// Make socket connectable by Firecracker's VMUser (mode 0660 if chowned, else 0666).
	if err := chownSockForUser(sockPath, vmUser); err != nil {
		_ = os.Chmod(sockPath, 0o666)
	}
	b := &gitAuthBridge{
		sockPath:   sockPath,
		targetSock: targetHostSock,
		ln:         ln,
	}
	go b.acceptLoop()
	return b, nil
}

func chownSockForUser(path, username string) error {
	if username == "" || os.Geteuid() != 0 {
		return os.ErrPermission
	}
	u, err := user.Lookup(username)
	if err != nil {
		return err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return err
	}
	if err := os.Chown(path, uid, gid); err != nil {
		return err
	}
	return os.Chmod(path, 0o660)
}

func (b *gitAuthBridge) acceptLoop() {
	for {
		conn, err := b.ln.Accept()
		if err != nil {
			return
		}
		go b.proxy(conn)
	}
}

func (b *gitAuthBridge) proxy(src net.Conn) {
	defer src.Close()
	dst, err := net.Dial("unix", b.targetSock)
	if err != nil {
		return
	}
	defer dst.Close()

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(dst, src)
		if uc, ok := dst.(*net.UnixConn); ok {
			_ = uc.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(src, dst)
		if uc, ok := src.(*net.UnixConn); ok {
			_ = uc.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done
	<-done
}

func (b *gitAuthBridge) Close() error {
	var err error
	b.closeOnce.Do(func() {
		err = b.ln.Close()
		_ = os.Remove(b.sockPath)
	})
	return err
}
