package gitauth

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// fakeClient mints one token for one repo and refuses every other.
type fakeClient struct {
	repo string
	tok  *pb.GitToken
}

func (f *fakeClient) MintGitToken(_ context.Context, req *pb.MintGitTokenRequest) (*pb.GitToken, error) {
	if req.GetRepo() != f.repo {
		return nil, api.ToConnect(api.Errorf(api.CodeNotFound, "no credential for %s", req.GetRepo()))
	}
	return f.tok, nil
}

func (f *fakeClient) ListGitCredentials(context.Context, *pb.ListGitCredentialsRequest) (*pb.ListGitCredentialsResponse, error) {
	return &pb.ListGitCredentialsResponse{}, nil
}

func (f *fakeClient) ProxyGitHubAPI(context.Context, *pb.ProxyGitHubAPIRequest) (*pb.ProxyGitHubAPIResponse, error) {
	return nil, errors.New("not proxied")
}

// startServer serves git authentication for client in a fresh directory.
func startServer(t *testing.T, client Client) *Server {
	t.Helper()
	s, err := Start(t.TempDir(), nil, client)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

// askCredential asks the server's socket for a credential as
// git-credential-helper does, returning the status and body.
func askCredential(t *testing.T, s *Server, input string) (int, string) {
	t.Helper()
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", s.SockPath())
		},
	}}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://git-auth/git-credential", strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("POST /git-credential: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

// The socket answers git's credential request with the token the
// controller minted for that repo, and a refusal git can tell apart from a
// network failure (ExtractError) for any other.
func TestServerMintsCredentialsPerRepo(t *testing.T) {
	s := startServer(t, &fakeClient{repo: "github.com/acme/app.git", tok: &pb.GitToken{Username: "x-access-token", Token: "tok-1"}})

	status, body := askCredential(t, s, "protocol=https\nhost=github.com\npath=acme/app.git\n\n")
	if status != http.StatusOK || body != "username=x-access-token\npassword=tok-1" {
		t.Fatalf("credential for acme/app = %d %q, want 200 with the minted token", status, body)
	}

	status, body = askCredential(t, s, "protocol=https\nhost=github.com\npath=acme/other.git\n\n")
	if status != http.StatusNotFound {
		t.Fatalf("credential for acme/other = %d %q, want 404", status, body)
	}
	msg, ok := ExtractError(errors.New("fatal: " + body + "\nmore"))
	if !ok || msg != "no credential for github.com/acme/other.git" {
		t.Fatalf("ExtractError(%q) = %q, %v; want the controller's refusal", body, msg, ok)
	}
}

// The gh shim installed on PATH is the embedded ghshim.py, executable.
func TestServerInstallsGHShim(t *testing.T) {
	s := startServer(t, &fakeClient{})
	ghPath := filepath.Join(s.BinDir(), "gh")
	fi, err := os.Stat(ghPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("gh shim mode = %v, want 0755", fi.Mode().Perm())
	}
	got, err := os.ReadFile(ghPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != ghShimScript || !strings.HasPrefix(ghShimScript, "#!/usr/bin/env python3\n") {
		t.Errorf("gh shim is not the embedded python script (starts %q)", string(got[:min(len(got), 40)]))
	}
}

// A chroot's environment names the helpers at ChrootDir, the host's at
// the server's own directory; both reach the same SSH proxy.
func TestServerEnvironments(t *testing.T) {
	s := startServer(t, &fakeClient{})
	chroot := strings.Join(s.ChrootEnv(), "\n")
	host := strings.Join(s.HostEnv(), "\n")
	for _, want := range []string{
		"HOTBED_GIT_CRED_SOCK=" + ChrootDir + "/git-cred.sock",
		"HOTBED_GIT_SSH_KEY=" + ChrootDir + "/ssh_id",
	} {
		if !strings.Contains(chroot, want) {
			t.Errorf("chroot env lacks %q:\n%s", want, chroot)
		}
	}
	if !strings.Contains(host, "HOTBED_GIT_CRED_SOCK="+s.SockPath()) {
		t.Errorf("host env lacks the socket %s:\n%s", s.SockPath(), host)
	}
	if strings.Contains(host, ChrootDir) {
		t.Errorf("host env names the chroot's directory:\n%s", host)
	}
}

func TestParseGitSSHCommand(t *testing.T) {
	for _, tc := range []struct {
		cmd, service, repo string
		wantErr            bool
	}{
		{cmd: "git-upload-pack 'semistrict/hotbed.git'", service: "git-upload-pack", repo: "semistrict/hotbed.git"},
		{cmd: "git receive-pack '/owner/repo.git'", service: "git-receive-pack", repo: "owner/repo.git"},
		{cmd: `git-upload-pack "o/r.git"`, service: "git-upload-pack", repo: "o/r.git"},
		{cmd: "git-upload-archive 'o/r.git'", wantErr: true},
		{cmd: "git-upload-pack '../etc/passwd'", wantErr: true},
	} {
		service, repo, err := parseGitSSHCommand(tc.cmd)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseGitSSHCommand(%q) = %q, %q; want an error", tc.cmd, service, repo)
			}
			continue
		}
		if err != nil || service != tc.service || repo != tc.repo {
			t.Errorf("parseGitSSHCommand(%q) = %q, %q, %v; want %q, %q", tc.cmd, service, repo, err, tc.service, tc.repo)
		}
	}
}
