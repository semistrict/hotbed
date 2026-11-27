// Package gitauth delegates a worker's GitHub access to the controller: git
// over HTTPS asks its credential helper, git over SSH goes through a loopback
// SSH proxy that speaks Smart HTTP upstream, and a `gh` shim proxies REST and
// GraphQL calls. Every token is minted by the controller per request and
// never stored on the worker.
package gitauth

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
	"github.com/semistrict/hotbed/internal/shell"
)

const (
	// ChrootDir is where the worker's git credential socket, SSH key,
	// helper scripts, and fake `gh` binary are bind-mounted inside chroot commands.
	ChrootDir = "/run/hotbed/git-auth"
	// errPrefix marks errors emitted by the centralized git credential
	// helper or SSH proxy so git callers (such as remoteHead) distinguish
	// missing or rejected cluster credentials from generic network failures.
	errPrefix = "hotbed-git-auth:"
	// maxUnixSocketPath stays below macOS's 104-byte and Linux's 108-byte
	// sockaddr_un.sun_path limit.
	maxUnixSocketPath = 95
)

// Client is what a worker calls on the controller to delegate GitHub
// access to every session and command: minting git tokens (HTTPS + SSH proxy),
// proxying GitHub REST/GraphQL calls (worker `gh` shim), and listing
// credentials metadata (`gh auth status`). hotbedv1connect.GitAuthServiceClient
// satisfies it directly.
type Client interface {
	MintGitToken(ctx context.Context, req *pb.MintGitTokenRequest) (*pb.GitToken, error)
	ListGitCredentials(ctx context.Context, req *pb.ListGitCredentialsRequest) (*pb.ListGitCredentialsResponse, error)
	ProxyGitHubAPI(ctx context.Context, req *pb.ProxyGitHubAPIRequest) (*pb.ProxyGitHubAPIResponse, error)
}

// Server exposes local delegation endpoints on the worker:
//   - a Unix-domain HTTP socket (`git-cred.sock`) serving git-credential(1)
//     requests and the worker `gh` shim (`/gh-api`, `/gh-auth-status`)
//   - a loopback SSH proxy (`gitSSHProxy`) bridging Git SSH (`git@github.com:...`
//     and `ssh://...`) to authenticated Smart HTTP
//   - a lightweight `bin/gh` shim placed first on PATH for every command
//
// Tokens are never cached on disk or baked into workspace snapshots.
type Server struct {
	dir        string
	binDir     string
	tempDir    bool
	sockPath   string
	helperPath string
	provider   Client
	listener   net.Listener
	srv        *http.Server
	sshProxy   *gitSSHProxy
}

// Start serves git authentication from <stateDir>/git-auth (or a short
// directory under /tmp, should that path be too long for a socket), its
// socket and SSH key owned by cred's user when the worker runs as root.
func Start(stateDir string, cred *syscall.Credential, provider Client) (*Server, error) {
	dir := filepath.Join(stateDir, "git-auth")
	tempDir := false
	sockPath := filepath.Join(dir, "git-cred.sock")
	if len(sockPath) > maxUnixSocketPath {
		tmp, err := os.MkdirTemp("/tmp", "hb-ga-")
		if err != nil {
			return nil, fmt.Errorf("create git-auth socket dir: %w", err)
		}
		dir = tmp
		tempDir = true
		sockPath = filepath.Join(dir, "git-cred.sock")
	} else {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create git-auth dir %s: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o755); err != nil {
			return nil, fmt.Errorf("chmod git-auth dir %s: %w", dir, err)
		}
	}

	cleanupDir := func() {
		if tempDir {
			_ = os.RemoveAll(dir)
		}
	}

	helperPath := filepath.Join(dir, "git-credential-helper")
	if err := os.WriteFile(helperPath, []byte(gitops.GitCredentialHelperScript("")), 0o755); err != nil {
		cleanupDir()
		return nil, fmt.Errorf("write git credential helper: %w", err)
	}
	if err := os.Chmod(helperPath, 0o755); err != nil {
		cleanupDir()
		return nil, fmt.Errorf("chmod git credential helper: %w", err)
	}

	binDir, err := writeGHShim(dir)
	if err != nil {
		cleanupDir()
		return nil, err
	}

	sshProxy, err := startGitSSHProxy(dir, cred, provider, nil)
	if err != nil {
		cleanupDir()
		return nil, err
	}

	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		_ = sshProxy.Close()
		cleanupDir()
		return nil, fmt.Errorf("listen on git-auth socket %s: %w", sockPath, err)
	}
	if cred != nil && os.Geteuid() == 0 {
		// Owned by the build user so both the build user and root can connect.
		if err := os.Chown(sockPath, int(cred.Uid), int(cred.Gid)); err != nil {
			ln.Close()
			_ = sshProxy.Close()
			cleanupDir()
			return nil, fmt.Errorf("chown git-auth socket %s: %w", sockPath, err)
		}
		if err := os.Chmod(sockPath, 0o660); err != nil {
			ln.Close()
			_ = sshProxy.Close()
			cleanupDir()
			return nil, fmt.Errorf("chmod git-auth socket %s: %w", sockPath, err)
		}
	} else {
		if err := os.Chmod(sockPath, 0o600); err != nil {
			ln.Close()
			_ = sshProxy.Close()
			cleanupDir()
			return nil, fmt.Errorf("chmod git-auth socket %s: %w", sockPath, err)
		}
	}

	s := &Server{
		dir:        dir,
		binDir:     binDir,
		tempDir:    tempDir,
		sockPath:   sockPath,
		helperPath: helperPath,
		provider:   provider,
		listener:   ln,
		sshProxy:   sshProxy,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /git-credential", s.handleGet)
	mux.HandleFunc("POST /git-credential/erase", s.handleErase)
	mux.HandleFunc("POST /gh-api", s.handleGHAPI)
	mux.HandleFunc("GET /gh-auth-status", s.handleGHAuthStatus)
	s.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			slog.Warn("git-auth socket server stopped", "err", err)
		}
	}()
	return s, nil
}

// Close stops serving and removes the socket.
func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := s.srv.Shutdown(ctx)
	if s.sshProxy != nil {
		_ = s.sshProxy.Close()
	}
	_ = os.Remove(s.sockPath)
	if s.tempDir {
		_ = os.RemoveAll(s.dir)
	}
	return err
}

// Dir is the directory a chroot sees at ChrootDir.
func (s *Server) Dir() string        { return s.dir }
func (s *Server) BinDir() string     { return s.binDir }
func (s *Server) SockPath() string   { return s.sockPath }
func (s *Server) HelperPath() string { return s.helperPath }
func (s *Server) SSHPort() int       { return s.sshProxy.port }

// HostEnv configures git (HTTPS + SSH) and `gh` running directly on the worker host.
func (s *Server) HostEnv() []string {
	return authEnv(s.helperPath, s.sockPath, s.sshProxy.helperPath, s.sshProxy.keyPath, s.sshProxy.port)
}

// ChrootEnv configures git (HTTPS + SSH) and `gh` running inside a chroot unit
// where s.dir is bound at ChrootDir.
func (s *Server) ChrootEnv() []string {
	return authEnv(
		ChrootDir+"/git-credential-helper",
		ChrootDir+"/git-cred.sock",
		ChrootDir+"/git-ssh",
		ChrootDir+"/ssh_id",
		s.sshProxy.port,
	)
}

// authEnv returns environment variables that configure git HTTPS to use
// hotbed's centralized credential helper (clearing any inherited host helpers)
// and git SSH to tunnel through hotbed's local Git SSH protocol proxy.
func authEnv(helperPath, sockPath, sshHelperPath, sshKeyPath string, sshPort int) []string {
	return []string{
		"GIT_TERMINAL_PROMPT=0",
		"HOTBED_GIT_CRED_SOCK=" + sockPath,
		"HOTBED_GIT_SSH_PORT=" + strconv.Itoa(sshPort),
		"HOTBED_GIT_SSH_KEY=" + sshKeyPath,
		"GIT_SSH_COMMAND=/bin/sh " + shell.Quote(sshHelperPath),
		"GIT_CONFIG_COUNT=4",
		"GIT_CONFIG_KEY_0=credential.useHttpPath",
		"GIT_CONFIG_VALUE_0=true",
		"GIT_CONFIG_KEY_1=credential.helper",
		"GIT_CONFIG_VALUE_1=",
		"GIT_CONFIG_KEY_2=credential.helper",
		"GIT_CONFIG_VALUE_2=!/bin/sh " + shell.Quote(helperPath),
		"GIT_CONFIG_KEY_3=ssh.variant",
		"GIT_CONFIG_VALUE_3=ssh",
	}
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	attrs, err := parseGitCredentialInput(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("read credential request: %v", err))
		return
	}
	proto := strings.ToLower(attrs["protocol"])
	if proto != "https" && proto != "http" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	host := strings.TrimSpace(attrs["host"])
	path := strings.TrimPrefix(strings.TrimSpace(attrs["path"]), "/")
	if host == "" || path == "" {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("incomplete git credential request (host=%q path=%q); credential.useHttpPath must be enabled", host, path))
		return
	}

	tok, err := s.provider.MintGitToken(r.Context(), &pb.MintGitTokenRequest{
		Repo: host + "/" + path,
	})
	if err != nil {
		ae := api.FromConnect(err)
		status := http.StatusBadGateway
		switch ae.Code {
		case api.CodeNotFound:
			status = http.StatusNotFound
		case api.CodeNotAuthorized:
			status = http.StatusForbidden
		case api.CodeInvalidArgs:
			status = http.StatusBadRequest
		}
		msg := ae.Message
		if ae.Suggestion != "" {
			msg += " (" + ae.Suggestion + ")"
		}
		writeError(w, status, msg)
		return
	}
	if tok.GetUsername() == "" || tok.GetToken() == "" ||
		strings.ContainsAny(tok.GetUsername(), "\r\n\x00") ||
		strings.ContainsAny(tok.GetToken(), "\r\n\x00") {
		writeError(w, http.StatusBadGateway, "controller returned an invalid git token")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "username=%s\npassword=%s", tok.Username, tok.Token)
}

func (s *Server) handleErase(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	clean := strings.Join(strings.Fields(strings.ReplaceAll(msg, "\n", " ")), " ")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, "%s %s", errPrefix, clean)
}

func parseGitCredentialInput(r io.Reader) (map[string]string, error) {
	attrs := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, "=")
		if ok {
			attrs[k] = v
		}
	}
	return attrs, sc.Err()
}

// ExtractError extracts the message after errPrefix from git's
// error output when the centralized credential helper or SSH proxy refused or
// lacked credentials.
func ExtractError(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	s := err.Error()
	_, after, ok := strings.Cut(s, errPrefix)
	if !ok {
		return "", false
	}
	line, _, _ := strings.Cut(after, "\n")
	return strings.TrimSpace(line), true
}
