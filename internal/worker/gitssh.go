package worker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
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

	"golang.org/x/crypto/ssh"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// gitSSHWrapperScript is invoked by git via GIT_SSH_COMMAND=/bin/sh <path>.
// It parses git's SSH options (-p, -o, etc.), extracts the destination host
// (and optional custom port), and tunnels the git command over SSH to the
// worker's local Git SSH proxy on 127.0.0.1:$HOTBED_GIT_SSH_PORT.
const gitSSHWrapperScript = `#!/bin/sh
set -eu
target_port=""
while [ $# -gt 0 ]; do
  case "$1" in
    -p)
      target_port="$2"
      shift 2
      ;;
    -p*)
      target_port="${1#-p}"
      shift
      ;;
    -o|-i|-l|-F|-c|-b|-E|-I|-J|-L|-m|-O|-Q|-R|-S|-W|-w)
      shift 2
      ;;
    -o*|-i*|-l*|-F*|-4|-6|-A|-a|-C|-f|-G|-g|-K|-k|-M|-N|-n|-q|-s|-T|-t|-V|-v|-X|-x|-Y|-y)
      shift
      ;;
    --)
      shift
      break
      ;;
    -*)
      shift
      ;;
    *)
      break
      ;;
  esac
done
if [ $# -lt 2 ]; then
  echo "hotbed-git-auth: invalid ssh invocation" >&2
  exit 1
fi
dest="$1"
shift
cmd="$*"
host="${dest#*@}"
if [ -n "$target_port" ] && [ "$target_port" != "22" ]; then
  ssh_user="${host}:${target_port}"
else
  ssh_user="${host}"
fi
if [ -n "${GIT_PROTOCOL:-}" ]; then
  exec ssh \
    -i "$HOTBED_GIT_SSH_KEY" \
    -o IdentitiesOnly=yes \
    -o BatchMode=yes \
    -o StrictHostKeyChecking=no \
    -o UserKnownHostsFile=/dev/null \
    -o LogLevel=ERROR \
    -o "SetEnv=GIT_PROTOCOL=${GIT_PROTOCOL}" \
    -p "$HOTBED_GIT_SSH_PORT" \
    -l "$ssh_user" \
    127.0.0.1 \
    "$cmd"
else
  exec ssh \
    -i "$HOTBED_GIT_SSH_KEY" \
    -o IdentitiesOnly=yes \
    -o BatchMode=yes \
    -o StrictHostKeyChecking=no \
    -o UserKnownHostsFile=/dev/null \
    -o LogLevel=ERROR \
    -p "$HOTBED_GIT_SSH_PORT" \
    -l "$ssh_user" \
    127.0.0.1 \
    "$cmd"
fi
`

// gitSSHProxy runs a loopback SSH server on the worker that accepts
// git-upload-pack and git-receive-pack commands (e.g. git@github.com:org/repo.git
// or ssh://git@github.com/org/repo.git), mints credentials from the controller,
// and bridges the Git SSH wire protocol (v2 and v1) to upstream Smart HTTP.
type gitSSHProxy struct {
	listener   net.Listener
	port       int
	keyPath    string
	helperPath string
	provider   GitAuthClient
	httpClient *http.Client
	sshCfg     *ssh.ServerConfig
}

func startGitSSHProxy(dir string, cred *syscall.Credential, provider GitAuthClient, hc *http.Client) (*gitSSHProxy, error) {
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate git-ssh host key: %w", err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		return nil, fmt.Errorf("create git-ssh host signer: %w", err)
	}

	clientPub, clientPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate git-ssh client key: %w", err)
	}
	clientSSHPub, err := ssh.NewPublicKey(clientPub)
	if err != nil {
		return nil, fmt.Errorf("encode git-ssh client public key: %w", err)
	}
	clientKeyPEM, err := ssh.MarshalPrivateKey(clientPriv, "hotbed-git-ssh")
	if err != nil {
		return nil, fmt.Errorf("marshal git-ssh client key: %w", err)
	}

	keyPath := filepath.Join(dir, "ssh_id")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(clientKeyPEM), 0o600); err != nil {
		return nil, fmt.Errorf("write git-ssh key: %w", err)
	}
	if cred != nil && os.Geteuid() == 0 {
		if err := os.Chown(keyPath, int(cred.Uid), int(cred.Gid)); err != nil {
			return nil, fmt.Errorf("chown git-ssh key: %w", err)
		}
	}
	if err := os.Chmod(keyPath, 0o600); err != nil {
		return nil, fmt.Errorf("chmod git-ssh key: %w", err)
	}

	helperPath := filepath.Join(dir, "git-ssh")
	if err := os.WriteFile(helperPath, []byte(gitSSHWrapperScript), 0o755); err != nil {
		return nil, fmt.Errorf("write git-ssh wrapper: %w", err)
	}
	if err := os.Chmod(helperPath, 0o755); err != nil {
		return nil, fmt.Errorf("chmod git-ssh wrapper: %w", err)
	}

	authorizedBlob := clientSSHPub.Marshal()
	sshCfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(key.Marshal(), authorizedBlob) {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("unauthorized ssh client key")
		},
	}
	sshCfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen on git-ssh proxy: %w", err)
	}
	if hc == nil {
		hc = &http.Client{}
	}
	p := &gitSSHProxy{
		listener:   ln,
		port:       ln.Addr().(*net.TCPAddr).Port,
		keyPath:    keyPath,
		helperPath: helperPath,
		provider:   provider,
		httpClient: hc,
		sshCfg:     sshCfg,
	}
	go p.serve()
	return p, nil
}

func (p *gitSSHProxy) Close() error {
	return p.listener.Close()
}

func (p *gitSSHProxy) serve() {
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				slog.Warn("git-ssh proxy accept failed", "err", err)
			}
			return
		}
		go p.handleConn(conn)
	}
}

func (p *gitSSHProxy) handleConn(rawConn net.Conn) {
	defer rawConn.Close()
	sconn, chans, reqs, err := ssh.NewServerConn(rawConn, p.sshCfg)
	if err != nil {
		return
	}
	defer sconn.Close()
	go ssh.DiscardRequests(reqs)

	targetHost := strings.TrimSpace(sconn.User())
	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			_ = newChan.Reject(ssh.UnknownChannelType, "only session channels are supported")
			continue
		}
		ch, chReqs, err := newChan.Accept()
		if err != nil {
			continue
		}
		go p.handleSession(targetHost, ch, chReqs)
	}
}

type sshEnvRequest struct {
	Name  string
	Value string
}

type sshExecRequest struct {
	Command string
}

type sshExitStatus struct {
	Status uint32
}

func (p *gitSSHProxy) handleSession(targetHost string, ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()

	var gitProtocol string
	for req := range reqs {
		switch req.Type {
		case "env":
			var env sshEnvRequest
			if err := ssh.Unmarshal(req.Payload, &env); err == nil && env.Name == "GIT_PROTOCOL" {
				gitProtocol = env.Value
				if req.WantReply {
					_ = req.Reply(true, nil)
				}
			} else if req.WantReply {
				_ = req.Reply(false, nil)
			}
		case "exec":
			var execReq sshExecRequest
			if err := ssh.Unmarshal(req.Payload, &execReq); err != nil {
				if req.WantReply {
					_ = req.Reply(false, nil)
				}
				return
			}
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
			code := p.runGitCommand(context.Background(), targetHost, gitProtocol, execReq.Command, ch)
			_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(sshExitStatus{Status: code}))
			return
		default:
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
}

func (p *gitSSHProxy) runGitCommand(ctx context.Context, host, gitProtocol, cmd string, ch ssh.Channel) uint32 {
	service, repoPath, err := parseGitSSHCommand(cmd)
	if err != nil {
		fmt.Fprintf(ch.Stderr(), "%s %v\n", gitAuthErrPrefix, err)
		return 128
	}
	if host == "" || strings.ContainsAny(host, " \t\r\n/@") {
		fmt.Fprintf(ch.Stderr(), "%s invalid SSH target host %q\n", gitAuthErrPrefix, host)
		return 128
	}

	var authHeader string
	var mintErrMsg string
	tok, err := p.provider.MintGitToken(ctx, &pb.MintGitTokenRequest{
		Repo: host + "/" + repoPath,
	})
	if err != nil {
		ae := api.FromConnect(err)
		msg := ae.Message
		if ae.Suggestion != "" {
			msg += " (" + ae.Suggestion + ")"
		}
		if ae.Code != api.CodeNotFound {
			fmt.Fprintf(ch.Stderr(), "%s %s\n", gitAuthErrPrefix, msg)
			return 128
		}
		// No credential configured for this repo: try unauthenticated Smart HTTP
		// first so public SSH repos still work, and report mintErrMsg if the
		// remote requires auth.
		mintErrMsg = msg
	} else {
		authHeader = "Basic " + base64.StdEncoding.EncodeToString([]byte(tok.GetUsername()+":"+tok.GetToken()))
	}

	scheme := "https"
	if isLoopbackHost(host) {
		scheme = "http"
	}
	baseURL := fmt.Sprintf("%s://%s/%s", scheme, host, repoPath)

	adv, isV2, status, err := p.fetchAdvertisement(ctx, baseURL, service, gitProtocol, authHeader)
	if err != nil {
		if status == http.StatusUnauthorized || status == http.StatusForbidden || (status == http.StatusNotFound && mintErrMsg != "") {
			if mintErrMsg != "" {
				fmt.Fprintf(ch.Stderr(), "%s %s\n", gitAuthErrPrefix, mintErrMsg)
			} else {
				fmt.Fprintf(ch.Stderr(), "%s remote rejected git credentials for %s/%s: %v\n", gitAuthErrPrefix, host, repoPath, err)
			}
		} else {
			fmt.Fprintf(ch.Stderr(), "fatal: %v\n", err)
		}
		return 128
	}
	if _, err := ch.Write(adv); err != nil {
		return 128
	}

	br := bufio.NewReader(ch)
	switch {
	case isV2 && service == "git-upload-pack":
		if err := p.proxyUploadPackV2(ctx, baseURL, gitProtocol, authHeader, br, ch); err != nil {
			fmt.Fprintf(ch.Stderr(), "fatal: %v\n", err)
			return 128
		}
	case service == "git-upload-pack":
		if err := p.proxyUploadPackV1(ctx, baseURL, authHeader, br, ch); err != nil {
			fmt.Fprintf(ch.Stderr(), "fatal: %v\n", err)
			return 128
		}
	case service == "git-receive-pack":
		if err := p.proxyReceivePackV1(ctx, baseURL, authHeader, br, ch); err != nil {
			fmt.Fprintf(ch.Stderr(), "fatal: %v\n", err)
			return 128
		}
	}
	return 0
}

func (p *gitSSHProxy) fetchAdvertisement(ctx context.Context, baseURL, service, gitProtocol, authHeader string) (adv []byte, isV2 bool, status int, err error) {
	u := fmt.Sprintf("%s/info/refs?service=%s", baseURL, service)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, false, 0, err
	}
	req.Header.Set("User-Agent", "git/2.45.0")
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	if gitProtocol != "" {
		req.Header.Set("Git-Protocol", gitProtocol)
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, false, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = resp.Status
		}
		return nil, false, resp.StatusCode, fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, u, msg)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, false, resp.StatusCode, err
	}
	stripped := stripSmartHTTPPreamble(raw)
	isV2 = bytes.HasPrefix(stripped, []byte("000eversion 2\n"))
	return stripped, isV2, resp.StatusCode, nil
}

// stripSmartHTTPPreamble removes the leading `# service=<svc>\n` pkt-line and
// following `0000` flush-pkt from a Smart HTTP /info/refs response so the
// remaining bytes match the native Git SSH advertisement.
func stripSmartHTTPPreamble(raw []byte) []byte {
	br := bufio.NewReader(bytes.NewReader(raw))
	kind, payload, _, err := readPktLine(br)
	if err != nil || kind != pktData || !bytes.HasPrefix(payload, []byte("# service=")) {
		return raw
	}
	kind, _, _, err = readPktLine(br)
	if err != nil || kind != pktFlush {
		return raw
	}
	rest, err := io.ReadAll(br)
	if err != nil {
		return raw
	}
	return rest
}

func (p *gitSSHProxy) proxyUploadPackV2(ctx context.Context, baseURL, gitProtocol, authHeader string, br *bufio.Reader, out io.Writer) error {
	for {
		var reqBuf bytes.Buffer
		hasData := false
		for {
			kind, _, raw, err := readPktLine(br)
			if errors.Is(err, io.EOF) {
				if !hasData {
					return nil
				}
				return io.ErrUnexpectedEOF
			}
			if err != nil {
				return err
			}
			reqBuf.Write(raw)
			if kind == pktFlush {
				break
			}
			hasData = true
		}
		if !hasData {
			return nil
		}
		if err := p.postSmartHTTP(ctx, baseURL+"/git-upload-pack", "git-upload-pack", gitProtocol, authHeader, &reqBuf, out); err != nil {
			return err
		}
	}
}

func (p *gitSSHProxy) proxyUploadPackV1(ctx context.Context, baseURL, authHeader string, br *bufio.Reader, out io.Writer) error {
	var wants bytes.Buffer
	hasWants := false
	for {
		kind, _, raw, err := readPktLine(br)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		wants.Write(raw)
		if kind == pktFlush {
			break
		}
		hasWants = true
	}
	if !hasWants {
		return nil
	}

	var allHaves bytes.Buffer
	for {
		var term []byte
		done := false
		for {
			kind, payload, raw, err := readPktLine(br)
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			if kind == pktFlush {
				term = raw
				break
			}
			if kind == pktData && bytes.Equal(bytes.TrimSpace(payload), []byte("done")) {
				term = raw
				done = true
				break
			}
			allHaves.Write(raw)
		}
		body := io.MultiReader(
			bytes.NewReader(wants.Bytes()),
			bytes.NewReader(allHaves.Bytes()),
			bytes.NewReader(term),
		)
		if err := p.postSmartHTTP(ctx, baseURL+"/git-upload-pack", "git-upload-pack", "", authHeader, body, out); err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

func (p *gitSSHProxy) proxyReceivePackV1(ctx context.Context, baseURL, authHeader string, br *bufio.Reader, out io.Writer) error {
	var cmds bytes.Buffer
	hasCmds := false
	hasNonDelete := false
	for {
		kind, payload, raw, err := readPktLine(br)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		cmds.Write(raw)
		if kind == pktFlush {
			break
		}
		if kind == pktData {
			hasCmds = true
			line := payload
			if nul := bytes.IndexByte(line, 0); nul >= 0 {
				line = line[:nul]
			}
			fields := strings.Fields(string(line))
			if len(fields) >= 2 && !isZeroOID(fields[1]) {
				hasNonDelete = true
			}
		}
	}
	if !hasCmds {
		return nil
	}

	var body io.Reader = bytes.NewReader(cmds.Bytes())
	if hasNonDelete {
		body = io.MultiReader(bytes.NewReader(cmds.Bytes()), br)
	}
	return p.postSmartHTTP(ctx, baseURL+"/git-receive-pack", "git-receive-pack", "", authHeader, body, out)
}

func (p *gitSSHProxy) postSmartHTTP(ctx context.Context, targetURL, service, gitProtocol, authHeader string, body io.Reader, out io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, body)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "git/2.45.0")
	req.Header.Set("Content-Type", fmt.Sprintf("application/x-%s-request", service))
	req.Header.Set("Accept", fmt.Sprintf("application/x-%s-result", service))
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	if gitProtocol != "" {
		req.Header.Set("Git-Protocol", gitProtocol)
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("HTTP %d from %s: %s", resp.StatusCode, targetURL, strings.TrimSpace(string(msg)))
	}
	_, err = io.Copy(out, resp.Body)
	return err
}

type pktKind int

const (
	pktData pktKind = iota
	pktFlush
	pktDelim
	pktResponseEnd
)

func readPktLine(r io.Reader) (kind pktKind, payload, raw []byte, err error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, nil, err
	}
	var lenBytes [2]byte
	if _, err := hex.Decode(lenBytes[:], hdr[:]); err != nil {
		return 0, nil, nil, fmt.Errorf("invalid git pkt-line length %q", string(hdr[:]))
	}
	n := int(lenBytes[0])<<8 | int(lenBytes[1])
	switch n {
	case 0:
		return pktFlush, nil, append([]byte(nil), hdr[:]...), nil
	case 1:
		return pktDelim, nil, append([]byte(nil), hdr[:]...), nil
	case 2:
		return pktResponseEnd, nil, append([]byte(nil), hdr[:]...), nil
	case 3:
		return 0, nil, nil, fmt.Errorf("invalid git pkt-line length 0003")
	}
	raw = make([]byte, n)
	copy(raw[:4], hdr[:])
	if _, err := io.ReadFull(r, raw[4:]); err != nil {
		return 0, nil, nil, err
	}
	return pktData, raw[4:], raw, nil
}

// parseGitSSHCommand parses a remote git SSH command such as
// `git-upload-pack 'semistrict/hotbed.git'` or `git-receive-pack '/owner/repo.git'`.
func parseGitSSHCommand(cmd string) (service, repoPath string, err error) {
	s := strings.TrimSpace(cmd)
	var rest string
	switch {
	case strings.HasPrefix(s, "git-upload-pack "):
		service, rest = "git-upload-pack", strings.TrimSpace(strings.TrimPrefix(s, "git-upload-pack "))
	case strings.HasPrefix(s, "git upload-pack "):
		service, rest = "git-upload-pack", strings.TrimSpace(strings.TrimPrefix(s, "git upload-pack "))
	case strings.HasPrefix(s, "git-receive-pack "):
		service, rest = "git-receive-pack", strings.TrimSpace(strings.TrimPrefix(s, "git-receive-pack "))
	case strings.HasPrefix(s, "git receive-pack "):
		service, rest = "git-receive-pack", strings.TrimSpace(strings.TrimPrefix(s, "git receive-pack "))
	default:
		return "", "", fmt.Errorf("only git-upload-pack and git-receive-pack are supported over hotbed's Git SSH proxy (got %q)", cmd)
	}
	repoPath, err = unquoteGitArg(rest)
	if err != nil {
		return "", "", err
	}
	repoPath = strings.TrimPrefix(strings.TrimSpace(repoPath), "/")
	if repoPath == "" || strings.ContainsAny(repoPath, " \t\r\n?#") || strings.Contains(repoPath, "..") {
		return "", "", fmt.Errorf("invalid git repository path %q", repoPath)
	}
	return service, repoPath, nil
}

func unquoteGitArg(s string) (string, error) {
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		inner := s[1 : len(s)-1]
		// Git escapes embedded single quotes as '\''
		return strings.ReplaceAll(inner, `'\''`, `'`), nil
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return strconv.Unquote(s)
	}
	return s, nil
}

func isZeroOID(oid string) bool {
	if len(oid) < 40 {
		return false
	}
	for i := range len(oid) {
		if oid[i] != '0' {
			return false
		}
	}
	return true
}

func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(strings.ToLower(host), "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
