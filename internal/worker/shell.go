package worker

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/httpx"
	"github.com/semistrict/hotbed/internal/unit"
)

const (
	shellTicketTTL       = 60 * time.Second
	shellHandshakeBudget = 15 * time.Second
	shellDisconnectGrace = 2 * time.Second
)

type shellTicket struct {
	id          string
	workspaceID string
	sessionID   string
	repo        string
	args        []string
	env         []string
	pubKey      ssh.PublicKey
	expiresAt   time.Time
	// claimedBy holds the SSH connection session ID that passed PublicKeyCallback
	// (OpenSSH queries pubkey acceptance before sending the signed auth request),
	// so no second connection can race the same ticket before takeShellTicket.
	claimedBy []byte
}

type activeShell struct {
	id          string
	workspaceID string
	sessionID   string
	cancel      context.CancelFunc
	done        chan struct{}
}

func newShellHostKey() (ssh.Signer, string, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", fmt.Errorf("generate ssh host key: %w", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, "", fmt.Errorf("make ssh host signer: %w", err)
	}
	pub := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	return signer, pub, nil
}

// SetShellListen records the address and port this worker's SSH shell server
// listens on, so OpenShell can tell clients where to connect.
func (e *Engine) SetShellListen(addr string, port int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.shellAddr = addr
	e.shellPort = port
}

// OpenShell issues a single-use, short-lived SSH ticket for an interactive
// shell (or command) in a workspace on this worker.
func (e *Engine) OpenShell(ctx context.Context, req *pb.OpenShellRequest) (*pb.OpenShellResponse, error) {
	if strings.TrimSpace(req.WorkspaceId) == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "workspace_id is required")
	}
	if err := validateID(req.WorkspaceId); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.PublicKey) == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "public_key is required")
	}
	pubKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(req.PublicKey))
	if err != nil {
		return nil, api.Errorf(api.CodeInvalidArgs, "invalid SSH public_key: %v", err)
	}
	for _, kv := range req.Env {
		if !strings.Contains(kv, "=") {
			return nil, api.Errorf(api.CodeInvalidArgs, "invalid env entry %q: want KEY=VALUE", kv)
		}
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.shellPort == 0 && e.shellAddr == "" {
		return nil, api.Errorf(api.CodePlatformNotSupported, "this worker is not serving SSH shells").
			WithSuggestion("upgrade or restart the worker so its SSH shell listener starts")
	}
	w, ok := e.workspaces[req.WorkspaceId]
	if !ok || w.ws().State == released || w.releasing {
		return nil, api.Errorf(api.CodeNotFound, "no workspace %s on this node", req.WorkspaceId)
	}
	switch w.ws().State {
	case preparing:
		return nil, &api.Error{Code: api.CodeConflict, Message: "workspace " + req.WorkspaceId + " is still preparing: " + w.ws().Reason, Retryable: true}
	case failedWS:
		return nil, api.Errorf(api.CodeWorkspaceFailed, "workspace %s failed: %s", req.WorkspaceId, w.ws().Reason).
			WithSuggestion("run again: the controller releases a failed workspace and gives the session a new one")
	}
	if req.Repo != "" && repoOf(w.ws(), req.Repo) == nil {
		return nil, api.Errorf(api.CodeInvalidArgs, "workspace %s of project %s has no repo %s", req.WorkspaceId, w.ws().Project, req.Repo).
			WithSuggestion("hotbed project ls shows each project's repos; hotbed repo add puts a repo in one")
	}

	now := e.now()
	for k, t := range e.shellTickets {
		if !now.Before(t.expiresAt) {
			delete(e.shellTickets, k)
		}
	}

	var rnd [16]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return nil, fmt.Errorf("mint shell ticket: %w", err)
	}
	ticketID := "sh-" + hex.EncodeToString(rnd[:])
	expires := now.Add(shellTicketTTL)
	e.shellTickets[ticketID] = &shellTicket{
		id:          ticketID,
		workspaceID: req.WorkspaceId,
		sessionID:   w.ws().SessionId,
		repo:        req.Repo,
		args:        slices.Clone(req.Args),
		env:         slices.Clone(req.Env),
		pubKey:      pubKey,
		expiresAt:   expires,
	}
	if w.ws().SessionId != "" {
		e.active[w.ws().SessionId] = true
	}

	addr := e.shellAddrFor(ctx)
	return &pb.OpenShellResponse{
		Addr:          addr,
		User:          ticketID,
		HostPublicKey: e.shellHostPub,
		ExpiresAt:     expires.UnixMilli(),
	}, nil
}

// shellAddrFor returns the SSH host:port reachable on the same IP family the
// client used for the OpenShell HTTP call, falling back to e.shellAddr.
// Call with e.mu held.
func (e *Engine) shellAddrFor(ctx context.Context) string {
	if e.shellPort > 0 {
		if la, ok := ctx.Value(http.LocalAddrContextKey).(net.Addr); ok && la != nil {
			if ap, err := netip.ParseAddrPort(la.String()); err == nil {
				return net.JoinHostPort(ap.Addr().Unmap().String(), strconv.Itoa(e.shellPort))
			}
		}
	}
	return e.shellAddr
}

func (e *Engine) verifyShellTicket(user string, key ssh.PublicKey, sessionID []byte) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, ok := e.shellTickets[user]
	if !ok {
		return false
	}
	if !e.now().Before(t.expiresAt) {
		delete(e.shellTickets, user)
		return false
	}
	if !bytes.Equal(key.Marshal(), t.pubKey.Marshal()) {
		return false
	}
	if len(t.claimedBy) > 0 && !bytes.Equal(t.claimedBy, sessionID) {
		return false
	}
	t.claimedBy = slices.Clone(sessionID)
	return true
}

func (e *Engine) takeShellTicket(user string, sessionID []byte) (*shellTicket, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, ok := e.shellTickets[user]
	if !ok {
		return nil, false
	}
	if !e.now().Before(t.expiresAt) {
		delete(e.shellTickets, user)
		return nil, false
	}
	if len(t.claimedBy) > 0 && !bytes.Equal(t.claimedBy, sessionID) {
		return nil, false
	}
	delete(e.shellTickets, user)
	return t, true
}

// shellCacheEnv is cacheEnv without forcing CARGO_TERM_COLOR=never when
// running in an interactive PTY.
func shellCacheEnv(hasPTY bool) []string {
	env := cacheEnv()
	if !hasPTY {
		return env
	}
	return slices.DeleteFunc(env, func(kv string) bool {
		return strings.HasPrefix(kv, "CARGO_TERM_COLOR=")
	})
}

func (e *Engine) shellSpec(shellID string, w *workspace, repo string, args, extraEnv, userEnv []string, hasPTY bool) (unit.Spec, string) {
	ws := w.ws()
	volPath := e.cfg.Store.Path(w.rec.Volume)
	dir := WorkPath
	if repo != "" {
		dir += "/" + repo
	}
	tmpBase := filepath.Join(e.tmpRoot(w.rec.Volume), "sh-"+shellID)
	tmpDir := filepath.Join(tmpBase, "tmp")
	runDir := filepath.Join(tmpBase, "run")
	runtimeTarget := e.runtimeChrootDir()

	env := append(shellCacheEnv(hasPTY), "XDG_RUNTIME_DIR="+runtimeTarget)
	env = append(env, "HOTBED_WORKSPACE="+ws.Id)
	if ws.SessionId != "" {
		env = append(env, "HOTBED_SESSION="+ws.SessionId)
	}
	env = append(env, extraEnv...)
	env = append(env, userEnv...)

	spec := unit.Spec{
		Name:       "hotbed-sh-" + shellID,
		Slice:      e.cfg.Slice,
		Argv:       args,
		Dir:        dir,
		Binds:      []unit.Bind{{Source: runDir, Target: runtimeTarget}},
		Tmp:        tmpDir,
		User:       e.cfg.User,
		Env:        env,
		PathPrefix: cachePath,
	}
	e.inRootfs(&spec, volPath)
	return spec, tmpDir
}

func (e *Engine) makeShellTmp(vol, shellID string) error {
	return makeTmpDirs(e.cfg.Store.Path(vol), "sh-"+shellID, e.cred)
}

func (e *Engine) beginShell(ctx context.Context, ticket *shellTicket, args, extraEnv []string, sio unit.ShellIO) (unit.ShellHandle, func(), error) {
	shellID := newID()
	e.mu.Lock()
	w, ok := e.workspaces[ticket.workspaceID]
	if !ok || w.ws().State == released || w.releasing {
		e.mu.Unlock()
		return nil, nil, api.Errorf(api.CodeNotFound, "no workspace %s on this node", ticket.workspaceID)
	}
	if w.ws().State != activeWS && w.ws().State != idle {
		state, reason := w.ws().State, w.ws().Reason
		e.mu.Unlock()
		return nil, nil, api.Errorf(api.CodeConflict, "workspace %s is %s (%s)", ticket.workspaceID, state, reason)
	}
	spec, _ := e.shellSpec(shellID, w, ticket.repo, args, extraEnv, ticket.env, sio.PTY)
	vol := w.rec.Volume
	volPath := e.cfg.Store.Path(vol)
	sessionID := w.ws().SessionId
	shellCtx, cancel := context.WithCancel(ctx)
	as := &activeShell{
		id:          shellID,
		workspaceID: ticket.workspaceID,
		sessionID:   sessionID,
		cancel:      cancel,
		done:        make(chan struct{}),
	}
	e.shells[shellID] = as
	now := e.nowMs()
	w.ws().LastActiveAt = now
	if w.ws().State == idle {
		w.ws().State = activeWS
		e.saveWorkspace(w, change{what: "active", detail: "shell " + shellID})
	}
	if sessionID != "" {
		e.active[sessionID] = true
	}
	e.mu.Unlock()

	useRT := e.usesRuntime(ticket.workspaceID)
	cleanup := func() {
		cancel()
		if !useRT {
			removeTmp(volPath, "sh-"+shellID)
		}
		e.mu.Lock()
		delete(e.shells, shellID)
		if w, ok := e.workspaces[ticket.workspaceID]; ok {
			w.ws().LastActiveAt = e.nowMs()
			if w.ws().State == activeWS && !e.workspaceBusy(ticket.workspaceID) {
				w.ws().State = idle
				e.saveWorkspace(w, change{what: "idle", detail: "shell " + shellID + " ended"})
			}
		}
		e.mu.Unlock()
		close(as.done)
	}

	if !useRT {
		if err := e.makeShellTmp(vol, shellID); err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("make shell tmp: %w", err)
		}
	}
	handle, err := e.cfg.Units.StartShell(shellCtx, spec, sio)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return handle, cleanup, nil
}

// ShellServer serves interactive workspace shells over SSH behind the same
// fail-closed tailnet gate as the worker's HTTP API.
type ShellServer struct {
	id              httpx.Identifier
	policy          httpx.Policy
	eng             *Engine
	sshCfg          *ssh.ServerConfig
	disconnectGrace time.Duration // defaults to shellDisconnectGrace

	mu        sync.Mutex
	closed    bool
	listeners []net.Listener
	conns     map[net.Conn]struct{}
	wg        sync.WaitGroup
}

// NewShellServer constructs an SSH server for eng gated by id and policy.
func NewShellServer(id httpx.Identifier, policy httpx.Policy, eng *Engine) *ShellServer {
	s := &ShellServer{
		id:     id,
		policy: policy,
		eng:    eng,
		conns:  map[net.Conn]struct{}{},
	}
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if !eng.verifyShellTicket(c.User(), key, c.SessionID()) {
				return nil, errors.New("invalid or expired shell ticket")
			}
			return &ssh.Permissions{
				Extensions: map[string]string{"ticket": c.User()},
			}, nil
		},
	}
	cfg.AddHostKey(eng.shellHostKey)
	s.sshCfg = cfg
	return s
}

// Serve accepts SSH connections on l until l is closed.
func (s *ShellServer) Serve(l net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return net.ErrClosed
	}
	s.listeners = append(s.listeners, l)
	s.mu.Unlock()

	for {
		conn, err := l.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			conn.Close()
			return nil
		}
		s.conns[conn] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()

		go func() {
			defer s.wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.conns, conn)
				s.mu.Unlock()
				conn.Close()
			}()
			s.handleConn(conn)
		}()
	}
}

// Close closes every listener and active SSH connection and waits for
// handlers to finish.
func (s *ShellServer) Close() error {
	s.mu.Lock()
	s.closed = true
	ls := slices.Clone(s.listeners)
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()

	var errs []error
	for _, l := range ls {
		if err := l.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}
	}
	for _, c := range conns {
		_ = c.Close()
	}
	s.wg.Wait()
	return errors.Join(errs...)
}

func (s *ShellServer) handleConn(conn net.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), shellHandshakeBudget)
	defer cancel()
	if err := conn.SetDeadline(time.Now().Add(shellHandshakeBudget)); err != nil {
		return
	}
	if _, err := httpx.AdmitConn(ctx, conn, s.id, s.policy); err != nil {
		slog.Warn("refused an ssh shell connection", "remote", conn.RemoteAddr(), "why", err)
		return
	}
	sconn, chans, reqs, err := ssh.NewServerConn(conn, s.sshCfg)
	if err != nil {
		return
	}
	waitDone := make(chan struct{})
	var bgWG sync.WaitGroup
	defer func() {
		_ = sconn.Close()
		<-waitDone
		bgWG.Wait()
	}()
	if err := conn.SetDeadline(time.Time{}); err != nil {
		close(waitDone)
		return
	}
	ticketID := ""
	if sconn.Permissions != nil {
		ticketID = sconn.Permissions.Extensions["ticket"]
	}
	ticket, ok := s.eng.takeShellTicket(ticketID, sconn.SessionID())
	if !ok {
		close(waitDone)
		return
	}

	connCtx, connCancel := context.WithCancel(context.Background())
	defer connCancel()
	go func() {
		defer close(waitDone)
		_ = sconn.Wait()
		connCancel()
	}()
	bgWG.Add(1)
	go func() {
		defer bgWG.Done()
		ssh.DiscardRequests(reqs)
	}()

	served := false
	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			_ = newChan.Reject(ssh.UnknownChannelType, "only session channels are supported")
			continue
		}
		if served {
			_ = newChan.Reject(ssh.Prohibited, "only one session per shell ticket")
			continue
		}
		served = true
		ch, chReqs, err := newChan.Accept()
		if err != nil {
			return
		}
		peerClosed := s.serveSession(connCtx, ch, chReqs, ticket, &bgWG)
		s.waitClientDisconnect(chans, peerClosed, waitDone)
		return
	}
}

// waitClientDisconnect holds the transport open after the session ends until
// the client hangs up. The server never closes TCP before the client's
// CHANNEL_CLOSE arrives: SSH messages are ordered, so that close proves the
// client has read our EOF, exit-status and CHANNEL_CLOSE. Closing earlier
// lets the client's CHANNEL_CLOSE hit a closed socket, the kernel answers
// with RST, and OpenSSH dies writing SSH_MSG_DISCONNECT ("client_loop: send
// disconnect: Connection reset by peer"), exit 255 even though it got the
// exit status (board 8025, board 8316, board 9208). Only once the client has
// closed its side does the disconnect grace bound how long a client that
// never sends SSH_MSG_DISCONNECT keeps the connection; until then a slow
// client is as entitled to the connection as one sitting idle in a shell.
func (s *ShellServer) waitClientDisconnect(chans <-chan ssh.NewChannel, peerClosed, waitDone <-chan struct{}) {
	grace := s.disconnectGrace
	if grace <= 0 {
		grace = shellDisconnectGrace
	}
	var expired <-chan time.Time // nil until peerClosed: no deadline yet
	for {
		select {
		case <-waitDone:
			return
		case <-peerClosed:
			peerClosed = nil
			timer := time.NewTimer(grace)
			defer timer.Stop()
			expired = timer.C
		case newChan, ok := <-chans:
			if !ok {
				chans = nil // the transport is gone; waitDone follows
				continue
			}
			_ = newChan.Reject(ssh.Prohibited, "only one session per shell ticket")
		case <-expired:
			return
		}
	}
}

// serveSession runs the session on ch and returns once the server's side of
// the channel is closed (after EOF and exit-status). The returned channel is
// closed when the client's side is closed too: when chReqs, which x/crypto
// closes on the peer's CHANNEL_CLOSE or on transport loss, is exhausted.
func (s *ShellServer) serveSession(ctx context.Context, ch ssh.Channel, reqs <-chan *ssh.Request, ticket *shellTicket, bgWG *sync.WaitGroup) <-chan struct{} {
	peerClosed := make(chan struct{})
	reqsOwned := false // a goroutine has taken over reqs and closes peerClosed
	defer func() {
		_ = ch.Close()
		if reqsOwned {
			return
		}
		bgWG.Add(1)
		go func() {
			defer bgWG.Done()
			defer close(peerClosed)
			for r := range reqs {
				if r.WantReply {
					_ = r.Reply(false, nil)
				}
			}
		}()
	}()

	var (
		hasPTY   bool
		termName string
		cols     uint16 = 80
		rows     uint16 = 24
		extraEnv []string
	)

	var (
		handle  unit.ShellHandle
		cleanup func()
		started bool
	)

	for req := range reqs {
		switch req.Type {
		case "pty-req":
			if started {
				if req.WantReply {
					_ = req.Reply(false, nil)
				}
				continue
			}
			term, c, r, ok := parsePtyReq(req.Payload)
			if !ok {
				if req.WantReply {
					_ = req.Reply(false, nil)
				}
				continue
			}
			hasPTY, termName, cols, rows = true, term, c, r
			if req.WantReply {
				_ = req.Reply(true, nil)
			}

		case "env":
			if started {
				if req.WantReply {
					_ = req.Reply(false, nil)
				}
				continue
			}
			k, v, ok := parseEnvReq(req.Payload)
			if ok && allowedSSHEnv(k) {
				extraEnv = append(extraEnv, k+"="+v)
				if req.WantReply {
					_ = req.Reply(true, nil)
				}
			} else if req.WantReply {
				_ = req.Reply(false, nil)
			}

		case "window-change":
			c, r, ok := parseWindowChange(req.Payload)
			if ok {
				cols, rows = c, r
				if handle != nil {
					_ = handle.Resize(cols, rows)
				}
			}
			if req.WantReply {
				_ = req.Reply(ok, nil)
			}

		case "shell", "exec":
			if started {
				if req.WantReply {
					_ = req.Reply(false, nil)
				}
				continue
			}
			args := slices.Clone(ticket.args)
			if req.Type == "exec" && len(args) == 0 {
				if cmdStr, ok := parseExecReq(req.Payload); ok && cmdStr != "" {
					args = []string{"/bin/bash", "-c", cmdStr}
				}
			}
			sio := unit.ShellIO{
				PTY:  hasPTY,
				Cols: cols,
				Rows: rows,
				Term: termName,
			}
			if !hasPTY {
				sio.Stdin = ch
				sio.Stdout = ch
				sio.Stderr = ch.Stderr()
			}
			var err error
			handle, cleanup, err = s.eng.beginShell(ctx, ticket, args, extraEnv, sio)
			if err != nil {
				if req.WantReply {
					_ = req.Reply(false, nil)
				}
				fmt.Fprintf(ch.Stderr(), "hotbed: %v\n", err)
				_ = ch.CloseWrite()
				sendExitStatus(ch, 1)
				return peerClosed
			}
			started = true
			if req.WantReply {
				_ = req.Reply(true, nil)
			}

			var (
				resizeMu     sync.Mutex
				resizeClosed bool
			)
			// Keep handling window-change requests while the shell runs, and
			// until the client closes the channel.
			reqsOwned = true
			bgWG.Add(1)
			go func() {
				defer bgWG.Done()
				defer close(peerClosed)
				for r := range reqs {
					switch r.Type {
					case "window-change":
						c, rw, ok := parseWindowChange(r.Payload)
						if ok {
							resizeMu.Lock()
							if !resizeClosed {
								_ = handle.Resize(c, rw)
							}
							resizeMu.Unlock()
						}
						if r.WantReply {
							_ = r.Reply(ok, nil)
						}
					default:
						if r.WantReply {
							_ = r.Reply(false, nil)
						}
					}
				}
			}()

			var exitCode int
			if ptmx := handle.PTY(); ptmx != nil {
				outDone := make(chan struct{})
				go func() {
					defer close(outDone)
					_, _ = io.Copy(ch, ptmx)
				}()
				bgWG.Add(1)
				go func() {
					defer bgWG.Done()
					_, _ = io.Copy(ptmx, ch)
				}()
				exitCode = handle.Wait()
				// Once the process (and any background children) have exited, the
				// slave PTY is closed so io.Copy drains all remaining buffered bytes
				// and returns EOF/EIO; wait for drain, then close the master PTY.
				<-outDone
				resizeMu.Lock()
				resizeClosed = true
				resizeMu.Unlock()
				_ = handle.Close()
			} else {
				exitCode = handle.Wait()
				resizeMu.Lock()
				resizeClosed = true
				resizeMu.Unlock()
				_ = handle.Close()
			}
			cleanup()
			_ = ch.CloseWrite()
			sendExitStatus(ch, exitCode)
			return peerClosed

		default:
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
	return peerClosed
}

func sendExitStatus(ch ssh.Channel, code int) {
	if code < 0 {
		code = 1
	}
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, uint32(code))
	_, _ = ch.SendRequest("exit-status", false, payload)
}

func allowedSSHEnv(name string) bool {
	return name == "TERM" || name == "COLORTERM" || name == "LANG" || strings.HasPrefix(name, "LC_")
}

func parsePtyReq(b []byte) (term string, cols, rows uint16, ok bool) {
	term, rest, ok := readSSHString(b)
	if !ok || len(rest) < 8 {
		return "", 0, 0, false
	}
	c := binary.BigEndian.Uint32(rest[0:4])
	r := binary.BigEndian.Uint32(rest[4:8])
	return term, clampUint16(c), clampUint16(r), true
}

func parseWindowChange(b []byte) (cols, rows uint16, ok bool) {
	if len(b) < 8 {
		return 0, 0, false
	}
	c := binary.BigEndian.Uint32(b[0:4])
	r := binary.BigEndian.Uint32(b[4:8])
	return clampUint16(c), clampUint16(r), true
}

func parseEnvReq(b []byte) (name, val string, ok bool) {
	name, rest, ok := readSSHString(b)
	if !ok {
		return "", "", false
	}
	val, _, ok = readSSHString(rest)
	return name, val, ok
}

func parseExecReq(b []byte) (string, bool) {
	cmd, _, ok := readSSHString(b)
	return cmd, ok
}

func readSSHString(b []byte) (string, []byte, bool) {
	if len(b) < 4 {
		return "", nil, false
	}
	n := int(binary.BigEndian.Uint32(b[:4]))
	if n < 0 || len(b)-4 < n {
		return "", nil, false
	}
	return string(b[4 : 4+n]), b[4+n:], true
}

func clampUint16(v uint32) uint16 {
	if v > 0xffff {
		return 0xffff
	}
	return uint16(v)
}
