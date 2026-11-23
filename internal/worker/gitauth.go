package worker

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
	"github.com/semistrict/hotbed/internal/shell"
)

const (
	// gitAuthChrootDir is where the worker's git credential socket, SSH key,
	// helper scripts, and fake `gh` binary are bind-mounted inside chroot commands.
	gitAuthChrootDir = "/run/hotbed/git-auth"
	// gitAuthErrPrefix marks errors emitted by the centralized git credential
	// helper or SSH proxy so git callers (such as remoteHead) distinguish
	// missing or rejected cluster credentials from generic network failures.
	gitAuthErrPrefix = "hotbed-git-auth:"
	// maxUnixSocketPath stays below macOS's 104-byte and Linux's 108-byte
	// sockaddr_un.sun_path limit.
	maxUnixSocketPath = 95
)

// GitAuthClient is what a worker calls on the controller to delegate GitHub
// access to every session and command: minting git tokens (HTTPS + SSH proxy),
// proxying GitHub REST/GraphQL calls (worker `gh` shim), and listing
// credentials metadata (`gh auth status`). hotbedv1connect.GitAuthServiceClient
// satisfies it directly.
type GitAuthClient interface {
	MintGitToken(ctx context.Context, req *pb.MintGitTokenRequest) (*pb.GitToken, error)
	ListGitCredentials(ctx context.Context, req *pb.ListGitCredentialsRequest) (*pb.ListGitCredentialsResponse, error)
	ProxyGitHubAPI(ctx context.Context, req *pb.ProxyGitHubAPIRequest) (*pb.ProxyGitHubAPIResponse, error)
}

// gitAuthServer exposes local delegation endpoints on the worker:
//   - a Unix-domain HTTP socket (`git-cred.sock`) serving git-credential(1)
//     requests and the worker `gh` shim (`/gh-api`, `/gh-auth-status`)
//   - a loopback SSH proxy (`gitSSHProxy`) bridging Git SSH (`git@github.com:...`
//     and `ssh://...`) to authenticated Smart HTTP
//   - a lightweight `bin/gh` shim placed first on PATH for every command
//
// Tokens are never cached on disk or baked into workspace snapshots.
type gitAuthServer struct {
	dir        string
	binDir     string
	tempDir    bool
	sockPath   string
	helperPath string
	provider   GitAuthClient
	listener   net.Listener
	srv        *http.Server
	sshProxy   *gitSSHProxy
}

// gitCredentialHelperScript is invoked by git via GIT_CONFIG_COUNT. It reads
// git's key=value attributes on stdin, posts them to the worker's Unix socket,
// and writes credentials to stdout without ever touching disk.
const gitCredentialHelperScript = `#!/bin/sh
case "${1:-}" in
  get)   path=/git-credential ;;
  erase) path=/git-credential/erase ;;
  *)     exit 0 ;;
esac
out=$(curl -sS --unix-socket "$HOTBED_GIT_CRED_SOCK" -w '\n%{http_code}' --data-binary @- "http://localhost$path") || exit 1
nl='
'
code=${out##*"$nl"}
body=${out%"$nl"*}
if [ "$code" = "200" ]; then
  printf '%s\n' "$body"
  exit 0
fi
if [ "$code" = "204" ]; then
  exit 0
fi
printf '%s\n' "$body" >&2
exit 1
`

func startGitAuthServer(stateDir string, cred *syscall.Credential, provider GitAuthClient) (*gitAuthServer, error) {
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
	if err := os.WriteFile(helperPath, []byte(gitCredentialHelperScript), 0o755); err != nil {
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

	s := &gitAuthServer{
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

func (s *gitAuthServer) Close() error {
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

func (s *gitAuthServer) Dir() string        { return s.dir }
func (s *gitAuthServer) BinDir() string     { return s.binDir }
func (s *gitAuthServer) SockPath() string   { return s.sockPath }
func (s *gitAuthServer) HelperPath() string { return s.helperPath }
func (s *gitAuthServer) SSHPort() int       { return s.sshProxy.port }

// hostEnv configures git (HTTPS + SSH) and `gh` running directly on the worker host.
func (s *gitAuthServer) hostEnv() []string {
	return gitAuthEnv(s.helperPath, s.sockPath, s.sshProxy.helperPath, s.sshProxy.keyPath, s.sshProxy.port)
}

// chrootEnv configures git (HTTPS + SSH) and `gh` running inside a chroot unit
// where s.dir is bound at gitAuthChrootDir.
func (s *gitAuthServer) chrootEnv() []string {
	return gitAuthEnv(
		gitAuthChrootDir+"/git-credential-helper",
		gitAuthChrootDir+"/git-cred.sock",
		gitAuthChrootDir+"/git-ssh",
		gitAuthChrootDir+"/ssh_id",
		s.sshProxy.port,
	)
}

// gitAuthEnv returns environment variables that configure git HTTPS to use
// hotbed's centralized credential helper (clearing any inherited host helpers)
// and git SSH to tunnel through hotbed's local Git SSH protocol proxy.
func gitAuthEnv(helperPath, sockPath, sshHelperPath, sshKeyPath string, sshPort int) []string {
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

func (s *gitAuthServer) handleGet(w http.ResponseWriter, r *http.Request) {
	attrs, err := parseGitCredentialInput(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		writeGitAuthError(w, http.StatusBadRequest, fmt.Sprintf("read credential request: %v", err))
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
		writeGitAuthError(w, http.StatusBadRequest, fmt.Sprintf("incomplete git credential request (host=%q path=%q); credential.useHttpPath must be enabled", host, path))
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
		writeGitAuthError(w, status, msg)
		return
	}
	if tok.GetUsername() == "" || tok.GetToken() == "" ||
		strings.ContainsAny(tok.GetUsername(), "\r\n\x00") ||
		strings.ContainsAny(tok.GetToken(), "\r\n\x00") {
		writeGitAuthError(w, http.StatusBadGateway, "controller returned an invalid git token")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "username=%s\npassword=%s", tok.Username, tok.Token)
}

func (s *gitAuthServer) handleErase(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func writeGitAuthError(w http.ResponseWriter, status int, msg string) {
	clean := strings.Join(strings.Fields(strings.ReplaceAll(msg, "\n", " ")), " ")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, "%s %s", gitAuthErrPrefix, clean)
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

// extractGitAuthError extracts the message after gitAuthErrPrefix from git's
// error output when the centralized credential helper or SSH proxy refused or
// lacked credentials.
func extractGitAuthError(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	s := err.Error()
	_, after, ok := strings.Cut(s, gitAuthErrPrefix)
	if !ok {
		return "", false
	}
	line, _, _ := strings.Cut(after, "\n")
	return strings.TrimSpace(line), true
}
