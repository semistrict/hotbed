package worker_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/baseimage"
	"github.com/semistrict/hotbed/internal/fake"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/unit"
	"github.com/semistrict/hotbed/internal/worker"
)

type fakeGitAuthClient struct {
	mu      sync.Mutex
	calls   []string
	mintFn  func(ctx context.Context, req *pb.MintGitTokenRequest) (*pb.GitToken, error)
	listFn  func(ctx context.Context, req *pb.ListGitCredentialsRequest) (*pb.ListGitCredentialsResponse, error)
	proxyFn func(ctx context.Context, req *pb.ProxyGitHubAPIRequest) (*pb.ProxyGitHubAPIResponse, error)
}

func (f *fakeGitAuthClient) MintGitToken(ctx context.Context, req *pb.MintGitTokenRequest) (*pb.GitToken, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req.Repo)
	fn := f.mintFn
	f.mu.Unlock()
	if fn == nil {
		return nil, api.Errorf(api.CodeNotFound, "no credential")
	}
	return fn(ctx, req)
}

func (f *fakeGitAuthClient) ListGitCredentials(ctx context.Context, req *pb.ListGitCredentialsRequest) (*pb.ListGitCredentialsResponse, error) {
	if f.listFn == nil {
		return &pb.ListGitCredentialsResponse{}, nil
	}
	return f.listFn(ctx, req)
}

func (f *fakeGitAuthClient) ProxyGitHubAPI(ctx context.Context, req *pb.ProxyGitHubAPIRequest) (*pb.ProxyGitHubAPIResponse, error) {
	if f.proxyFn == nil {
		return nil, api.Errorf(api.CodeNotFound, "no proxy")
	}
	return f.proxyFn(ctx, req)
}

// newAuthGitServer serves a real bare git repository over Git Smart HTTP
// (both git-upload-pack and git-receive-pack), requiring HTTP Basic Auth
// (wantUser/wantPass) when wantPass != "".
func newAuthGitServer(t *testing.T, repoPath, wantUser, wantPass string) (repoURL, hostPort, bareDir string) {
	t.Helper()
	src := origin(t)
	bare := filepath.Join(t.TempDir(), "repo.git")
	if out, err := exec.Command("git", "clone", "--bare", "--quiet", src, bare).CombinedOutput(); err != nil {
		t.Fatalf("git clone --bare: %v: %s", err, out)
	}
	for _, kv := range [][2]string{
		{"uploadpack.allowFilter", "true"},
		{"uploadpack.allowAnySHA1InWant", "true"},
		{"http.receivepack", "true"},
	} {
		if out, err := exec.Command("git", "-C", bare, "config", kv[0], kv[1]).CombinedOutput(); err != nil {
			t.Fatalf("git config %s: %v: %s", kv[0], err, out)
		}
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if wantPass != "" {
			u, p, ok := r.BasicAuth()
			if !ok || u != wantUser || p != wantPass {
				w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
				http.Error(w, "authentication required", http.StatusUnauthorized)
				return
			}
		}
		var env []string
		if gp := r.Header.Get("Git-Protocol"); gp != "" {
			env = append(os.Environ(), "GIT_PROTOCOL="+gp)
		}
		svc := r.URL.Query().Get("service")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == repoPath+"/info/refs" && (svc == "git-upload-pack" || svc == "git-receive-pack"):
			sub := strings.TrimPrefix(svc, "git-")
			cmd := exec.CommandContext(r.Context(), "git", sub, "--stateless-rpc", "--advertise-refs", bare)
			cmd.Env = env
			out, err := cmd.Output()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", fmt.Sprintf("application/x-%s-advertisement", svc))
			hdr := fmt.Sprintf("# service=%s\n", svc)
			fmt.Fprintf(w, "%04x%s0000", len(hdr)+4, hdr)
			_, _ = w.Write(out)
		case r.Method == http.MethodPost && (r.URL.Path == repoPath+"/git-upload-pack" || r.URL.Path == repoPath+"/git-receive-pack"):
			sub := "upload-pack"
			svcName := "git-upload-pack"
			if strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
				sub = "receive-pack"
				svcName = "git-receive-pack"
			}
			var body io.Reader = r.Body
			if r.Header.Get("Content-Encoding") == "gzip" {
				gz, err := gzip.NewReader(r.Body)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				defer gz.Close()
				body = gz
			}
			cmd := exec.CommandContext(r.Context(), "git", sub, "--stateless-rpc", bare)
			cmd.Env = env
			cmd.Stdin = body
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				http.Error(w, stderr.String(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", fmt.Sprintf("application/x-%s-result", svcName))
			_, _ = w.Write(stdout.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts.URL + repoPath, strings.TrimPrefix(ts.URL, "http://"), bare
}

func runWorkspaceCmd(t *testing.T, eng *worker.Engine, dir, wsID, repo string, args ...string) (pb.CommandState, string) {
	t.Helper()
	cmd, err := eng.Exec(t.Context(), &pb.RunCommandRequest{
		WorkspaceId: wsID, Repo: repo, Args: args,
	})
	if err != nil {
		t.Fatalf("Exec(%v): %v", args, err)
	}
	var final pb.CommandState
	eventually(t, "command "+cmd.Id+" to finish", func() bool {
		c, ok := eng.Command(cmd.Id)
		if !ok {
			return false
		}
		if c.State == pb.CommandState_COMMAND_STATE_SUCCEEDED || c.State == pb.CommandState_COMMAND_STATE_FAILED {
			final = c.State
			return true
		}
		return false
	})
	logBytes, _ := os.ReadFile(filepath.Join(dir, "logs", cmd.Id+".log"))
	return final, string(logBytes)
}

func TestWorkerCentralizedGitAuthHTTPSAndSSHProxy(t *testing.T) {
	repoURL, hostPort, bareDir := newAuthGitServer(t, "/semistrict/hotbed.git", "x-access-token", "secret-tok-123")
	sshURL := "ssh://git@" + hostPort + "/semistrict/hotbed.git"

	provider := &fakeGitAuthClient{
		mintFn: func(_ context.Context, req *pb.MintGitTokenRequest) (*pb.GitToken, error) {
			if req.Repo != hostPort+"/semistrict/hotbed.git" {
				return nil, api.Errorf(api.CodeNotFound, "unexpected repo %q", req.Repo)
			}
			return &pb.GitToken{
				Host: hostPort, Repository: "semistrict/hotbed",
				Username: "x-access-token", Token: "secret-tok-123",
			}, nil
		},
	}

	dir := t.TempDir()
	store := &fake.Store{Root: filepath.Join(dir, "volumes")}
	units := &fake.Units{}
	eng, err := worker.NewEngine(t.Context(), worker.EngineConfig{
		Dir: dir, NodeID: "nMac", Host: "mac-worker",
		Store: store, BaseImage: fake.BaseImage(store), Units: units, System: fake.NewSystem(),
		GitAuth: provider, AllowLocalRepos: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { eng.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
		eng.Close()
	})

	// Create workspace from private HTTPS repo.
	proj := &pb.HostedProject{
		Name:  "hotbed",
		Repos: []*pb.Repo{{Id: "hotbed", Url: repoURL}},
		Steps: []*pb.Step{{Name: "check", Script: "test -f hotbed/lib.txt", Hash: "h1"}},
	}
	if _, err := eng.CreateWorkspace(ctx, &pb.CreateWorkspaceRequest{
		Id: "ws1", Repo: "hotbed", SessionId: "s1", Agent: "a", Project: proj,
	}); err != nil {
		t.Fatalf("CreateWorkspace against private repo failed: %v", err)
	}
	eventually(t, "private repo workspace ready", func() bool {
		ws, err := eng.Workspace("ws1")
		return err == nil && ws.State == pb.WorkspaceState_WORKSPACE_STATE_IDLE
	})

	// 1. HTTPS session git command works without per-node credentials.
	st, out := runWorkspaceCmd(t, eng, dir, "ws1", "hotbed", "git", "ls-remote", "origin", "HEAD")
	if st != pb.CommandState_COMMAND_STATE_SUCCEEDED || !strings.Contains(out, "HEAD") {
		t.Fatalf("HTTPS ls-remote failed (%v): %s", st, out)
	}

	// 2. Git SSH proxy (protocol v2): ls-remote, blobless clone, and push!
	st, out = runWorkspaceCmd(t, eng, dir, "ws1", "hotbed", "git", "ls-remote", sshURL, "HEAD")
	if st != pb.CommandState_COMMAND_STATE_SUCCEEDED || !strings.Contains(out, "HEAD") {
		t.Fatalf("SSH v2 ls-remote failed (%v): %s", st, out)
	}

	// 3. Git SSH proxy (protocol v1): ls-remote also works.
	st, out = runWorkspaceCmd(t, eng, dir, "ws1", "hotbed", "git", "-c", "protocol.version=1", "ls-remote", sshURL, "HEAD")
	if st != pb.CommandState_COMMAND_STATE_SUCCEEDED || !strings.Contains(out, "HEAD") {
		t.Fatalf("SSH v1 ls-remote failed (%v): %s", st, out)
	}

	// 4. Git SSH proxy: blobless clone + commit + push over real SSH protocol!
	pushScript := fmt.Sprintf(`set -euo pipefail
dst=$(mktemp -d)
git clone --quiet --filter=blob:none %q "$dst"
cd "$dst"
test -f lib.txt
git config user.name test
git config user.email test@example.com
echo "from-ssh-proxy" > ssh.txt
git add ssh.txt
git commit --quiet -m "push over ssh proxy"
git push --quiet origin HEAD:refs/heads/ssh-test
`, sshURL)
	st, out = runWorkspaceCmd(t, eng, dir, "ws1", "hotbed", "/bin/bash", "-c", pushScript)
	if st != pb.CommandState_COMMAND_STATE_SUCCEEDED {
		t.Fatalf("SSH clone+push failed (%v): %s", st, out)
	}
	// Verify pushed ref actually arrived in the remote bare repo.
	if refOut, err := exec.Command("git", "-C", bareDir, "rev-parse", "--verify", "refs/heads/ssh-test").CombinedOutput(); err != nil {
		t.Fatalf("pushed branch refs/heads/ssh-test missing from bare repo: %v: %s", err, refOut)
	}

	// 5. Verify the secret token was never written into .git/config.
	cfgBytes, err := os.ReadFile(filepath.Join(store.Path("ws/ws1"), "work", "hotbed", ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cfgBytes), "secret-tok-123") {
		t.Fatalf("token leaked into .git/config:\n%s", cfgBytes)
	}
}

func TestWorkerFakeGHShimDelegatesRESTAndGraphQL(t *testing.T) {
	var seen []*pb.ProxyGitHubAPIRequest
	var mu sync.Mutex
	provider := &fakeGitAuthClient{
		listFn: func(_ context.Context, _ *pb.ListGitCredentialsRequest) (*pb.ListGitCredentialsResponse, error) {
			return &pb.ListGitCredentialsResponse{
				Credentials: []*pb.GitCredential{
					{Name: "github", Host: "github.com", Username: "semistrict", Refreshable: true},
				},
			}, nil
		},
		proxyFn: func(_ context.Context, req *pb.ProxyGitHubAPIRequest) (*pb.ProxyGitHubAPIResponse, error) {
			mu.Lock()
			seen = append(seen, req)
			mu.Unlock()
			switch {
			case req.Path == "/user":
				return &pb.ProxyGitHubAPIResponse{StatusCode: 200, Body: `{"login":"semistrict"}`}, nil
			case req.Path == "/graphql":
				var payload map[string]any
				_ = json.Unmarshal([]byte(req.Body), &payload)
				vars, _ := payload["variables"].(map[string]any)
				return &pb.ProxyGitHubAPIResponse{
					StatusCode: 200,
					Body:       fmt.Sprintf(`{"data":{"viewer":{"login":"semistrict"},"org":%q}}`, vars["org"]),
				}, nil
			case req.Path == "/repos/semistrict/hotbed":
				return &pb.ProxyGitHubAPIResponse{
					StatusCode: 200,
					Body:       `{"full_name":"semistrict/hotbed","private":true,"default_branch":"main","html_url":"https://github.com/semistrict/hotbed"}`,
				}, nil
			case strings.HasPrefix(req.Path, "/repos/semistrict/hotbed/pulls"):
				return &pb.ProxyGitHubAPIResponse{
					StatusCode: 200,
					Body:       `[{"number":42,"title":"Central auth","state":"open","head":{"ref":"central-auth"}}]`,
				}, nil
			case strings.HasPrefix(req.Path, "/repos/semistrict/hotbed/issues"):
				return &pb.ProxyGitHubAPIResponse{
					StatusCode: 200,
					Body:       `[{"number":7,"title":"Real issue","state":"open"},{"number":42,"title":"PR","state":"open","pull_request":{}}]`,
				}, nil
			default:
				return &pb.ProxyGitHubAPIResponse{StatusCode: 404, Body: `{"message":"not found"}`}, nil
			}
		},
	}

	dir := t.TempDir()
	store := &fake.Store{Root: filepath.Join(dir, "volumes")}
	units := &fake.Units{}
	eng, err := worker.NewEngine(t.Context(), worker.EngineConfig{
		Dir: dir, NodeID: "nMac", Host: "mac-worker",
		Store: store, BaseImage: fake.BaseImage(store), Units: units, System: fake.NewSystem(),
		GitAuth: provider, AllowLocalRepos: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { eng.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
		eng.Close()
	})

	proj := &pb.HostedProject{
		Name:  "proj",
		Repos: []*pb.Repo{{Id: "app", Url: origin(t)}},
	}
	if _, err := eng.CreateWorkspace(ctx, &pb.CreateWorkspaceRequest{
		Id: "ws1", Repo: "app", SessionId: "s1", Agent: "a", Project: proj,
	}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "workspace ready", func() bool {
		ws, err := eng.Workspace("ws1")
		return err == nil && ws.State == pb.WorkspaceState_WORKSPACE_STATE_IDLE
	})

	// gh auth status shows controller metadata.
	st, out := runWorkspaceCmd(t, eng, dir, "ws1", "app", "gh", "auth", "status")
	if st != pb.CommandState_COMMAND_STATE_SUCCEEDED || !strings.Contains(out, "semistrict") || !strings.Contains(out, "hotbed controller") {
		t.Fatalf("gh auth status failed (%v): %s", st, out)
	}

	// gh auth token refuses to leak raw tokens onto workers.
	st, out = runWorkspaceCmd(t, eng, dir, "ws1", "app", "gh", "auth", "token")
	if st != pb.CommandState_COMMAND_STATE_FAILED || !strings.Contains(out, "stay on the hotbed controller") {
		t.Fatalf("gh auth token = (%v) %q, want failure explaining tokens stay on controller", st, out)
	}

	// gh api REST + GraphQL.
	st, out = runWorkspaceCmd(t, eng, dir, "ws1", "app", "gh", "api", "/user")
	if st != pb.CommandState_COMMAND_STATE_SUCCEEDED || !strings.Contains(out, `"semistrict"`) {
		t.Fatalf("gh api /user failed (%v): %s", st, out)
	}
	st, out = runWorkspaceCmd(t, eng, dir, "ws1", "app", "gh", "api", "graphql", "-f", "query=query { viewer { login } }", "-f", "org=semistrict")
	if st != pb.CommandState_COMMAND_STATE_SUCCEEDED || !strings.Contains(out, `"org":"semistrict"`) {
		t.Fatalf("gh api graphql failed (%v): %s", st, out)
	}

	// gh repo view / pr list / issue list.
	st, out = runWorkspaceCmd(t, eng, dir, "ws1", "app", "gh", "repo", "view", "semistrict/hotbed", "--json", "nameWithOwner,isPrivate")
	if st != pb.CommandState_COMMAND_STATE_SUCCEEDED || !strings.Contains(out, `"nameWithOwner": "semistrict/hotbed"`) && !strings.Contains(out, `"nameWithOwner":"semistrict/hotbed"`) {
		t.Fatalf("gh repo view failed (%v): %s", st, out)
	}
	st, out = runWorkspaceCmd(t, eng, dir, "ws1", "app", "gh", "pr", "list", "-R", "semistrict/hotbed", "--json", "number,headRefName")
	if st != pb.CommandState_COMMAND_STATE_SUCCEEDED || !strings.Contains(out, "central-auth") {
		t.Fatalf("gh pr list failed (%v): %s", st, out)
	}
	st, out = runWorkspaceCmd(t, eng, dir, "ws1", "app", "gh", "issue", "list", "-R", "semistrict/hotbed")
	if st != pb.CommandState_COMMAND_STATE_SUCCEEDED || !strings.Contains(out, "#7") || strings.Contains(out, "#42") {
		t.Fatalf("gh issue list failed (%v): %s", st, out)
	}
}

func TestWorkerMissingGitAuthFailsHardWithoutHostFallback(t *testing.T) {
	repoURL, hostPort, _ := newAuthGitServer(t, "/semistrict/hotbed.git", "x-access-token", "decoy-host-token")

	// Plant valid host credentials in HOME to prove the worker does NOT silently
	// fall back to per-node ~/.gitconfig or ~/.git-credentials.
	decoyHome := t.TempDir()
	credFile := filepath.Join(decoyHome, ".git-credentials")
	if err := os.WriteFile(credFile, []byte("http://x-access-token:decoy-host-token@"+hostPort+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(decoyHome, ".gitconfig"), []byte("[credential]\n\thelper = store --file="+credFile+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", decoyHome)

	provider := &fakeGitAuthClient{
		mintFn: func(_ context.Context, req *pb.MintGitTokenRequest) (*pb.GitToken, error) {
			return nil, api.Errorf(api.CodeNotFound, "no git credential configured for %s", req.Repo).
				WithSuggestion("run hotbed auth login")
		},
	}

	dir := t.TempDir()
	store := &fake.Store{Root: filepath.Join(dir, "volumes")}
	eng, err := worker.NewEngine(t.Context(), worker.EngineConfig{
		Dir: dir, NodeID: "nAsahi", Host: "asahimbp",
		Store: store, BaseImage: fake.BaseImage(store), Units: &fake.Units{}, System: fake.NewSystem(),
		GitAuth: provider,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })

	// HTTPS remote fails hard with non-retryable NOT_AUTHORIZED.
	_, err = eng.CreateWorkspace(t.Context(), &pb.CreateWorkspaceRequest{
		Id: "ws1", Repo: "hotbed", SessionId: "s1", Agent: "a",
		Project: &pb.HostedProject{Name: "hotbed", Repos: []*pb.Repo{{Id: "hotbed", Url: repoURL}}},
	})
	ae := api.FromConnect(err)
	if ae == nil || ae.Code != api.CodeNotAuthorized || ae.Retryable {
		t.Fatalf("CreateWorkspace HTTPS without controller credential = %+v, want non-retryable NOT_AUTHORIZED", ae)
	}
	if !strings.Contains(ae.Suggestion, "hotbed auth login") {
		t.Errorf("suggestion = %q, want hotbed auth login hint", ae.Suggestion)
	}

	// SSH remote also fails hard with non-retryable NOT_AUTHORIZED.
	sshURL := "ssh://git@" + hostPort + "/semistrict/hotbed.git"
	_, err = eng.CreateWorkspace(t.Context(), &pb.CreateWorkspaceRequest{
		Id: "ws2", Repo: "hotbed", SessionId: "s1", Agent: "a",
		Project: &pb.HostedProject{Name: "hotbed", Repos: []*pb.Repo{{Id: "hotbed", Url: sshURL}}},
	})
	ae = api.FromConnect(err)
	if ae == nil || ae.Code != api.CodeNotAuthorized || ae.Retryable {
		t.Fatalf("CreateWorkspace SSH without controller credential = %+v, want non-retryable NOT_AUTHORIZED", ae)
	}
}

func TestWorkerChrootSpecBindsGitAuthForAllCommandsAndNotHostCreds(t *testing.T) {
	dir := t.TempDir()
	store := &fake.Store{Root: filepath.Join(dir, "volumes")}
	units := &fake.Units{}
	baseMgr := &baseimage.Manager{
		Store: store,
		Spec:  baseimage.Spec{Image: "test-debian:trixie", Arch: "amd64", User: "hotbed", UID: 1000, GID: 1000, Script: "echo provision"},
		Pull: func(_ context.Context, _, _, rootDir string) error {
			return os.MkdirAll(filepath.Join(rootDir, "usr", "bin"), 0o755)
		},
		Provision: baseimage.ProvisionFunc(func(_ context.Context, rootDir, _ string) error {
			return nil
		}),
	}
	provider := &fakeGitAuthClient{}

	eng, err := worker.NewEngine(t.Context(), worker.EngineConfig{
		Dir: dir, NodeID: "nMac", Host: "mac-worker",
		Store: store, Units: units, System: fake.NewSystem(),
		BaseImage: baseMgr, GitAuth: provider, AllowLocalRepos: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { eng.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
		eng.Close()
	})

	proj := &pb.HostedProject{
		Name:  "proj1",
		Repos: []*pb.Repo{{Id: "app", Url: origin(t)}},
		Steps: []*pb.Step{{Name: "build", Script: "true", Hash: "h1"}},
	}
	if _, err := eng.CreateWorkspace(ctx, &pb.CreateWorkspaceRequest{
		Id: "ws1", Repo: "app", SessionId: "s1", Agent: "a", Project: proj,
	}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "workspace ready", func() bool {
		ws, err := eng.Workspace("ws1")
		return err == nil && ws.State == pb.WorkspaceState_WORKSPACE_STATE_IDLE
	})

	cmd, err := eng.Exec(ctx, &pb.RunCommandRequest{WorkspaceId: "ws1", Repo: "app", Args: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "command finished", func() bool {
		c, ok := eng.Command(cmd.Id)
		return ok && c.State == pb.CommandState_COMMAND_STATE_SUCCEEDED
	})
	spec, ok := units.Spec("hotbed-" + cmd.Id)
	if !ok {
		t.Fatalf("missing unit spec for %s", cmd.Id)
	}
	// Only the worker's git-auth directory and containers.conf defaults are
	// bound read-only: root of the sandbox may not rewrite helpers the worker
	// runs on the host (board 8145).
	if !slices.ContainsFunc(spec.ReadOnlyBinds, func(b unit.Bind) bool { return b.Target == "/run/hotbed/git-auth" }) {
		t.Errorf("chroot command spec read-only binds = %+v, want /run/hotbed/git-auth", spec.ReadOnlyBinds)
	}
	if !slices.ContainsFunc(spec.ReadOnlyBinds, func(b unit.Bind) bool { return b.Target == "/etc/containers/containers.conf.d/50-hotbed.conf" }) {
		t.Errorf("chroot command spec read-only binds = %+v, want /etc/containers/containers.conf.d/50-hotbed.conf", spec.ReadOnlyBinds)
	}
	if len(spec.ReadOnlyBinds) != 2 {
		t.Errorf("chroot command spec read-only binds = %+v, want only git-auth and containers.conf", spec.ReadOnlyBinds)
	}
	for _, b := range spec.Binds {
		if b.Target == "/run/hotbed/git-auth" || b.Target == "/etc/containers/containers.conf.d/50-hotbed.conf" {
			t.Errorf("chroot command spec binds %s read-write: %+v", b.Target, spec.Binds)
		}
	}
	if len(spec.PathPrefix) == 0 || spec.PathPrefix[0] != "/run/hotbed/git-auth/bin" {
		t.Errorf("chroot command spec PathPrefix = %v, want /run/hotbed/git-auth/bin first", spec.PathPrefix)
	}
	for _, wantEnv := range []string{
		"HOTBED_GIT_CRED_SOCK=/run/hotbed/git-auth/git-cred.sock",
		"HOTBED_GIT_SSH_KEY=/run/hotbed/git-auth/ssh_id",
		"GIT_CONFIG_KEY_0=credential.useHttpPath",
		"GIT_CONFIG_KEY_3=ssh.variant",
		"GIT_CONFIG_VALUE_3=ssh",
	} {
		if !slices.Contains(spec.Env, wantEnv) {
			t.Errorf("chroot command spec missing %q: %v", wantEnv, spec.Env)
		}
	}

	// The worker's own git in the checkout, in the rootfs, has the same
	// credentials: a checkout is a partial clone, which fetches what it
	// lacks from its remote (board 8333).
	if _, err := eng.Files(ctx, "ws1"); err != nil {
		t.Fatal(err)
	}
	execs := units.Execs()
	if len(execs) == 0 {
		t.Fatal("Files ran no git in the rootfs")
	}
	x := execs[len(execs)-1]
	gitAuthBind := slices.IndexFunc(x.Config.ReadOnlyBinds, func(b unit.Bind) bool { return b.Target == "/run/hotbed/git-auth" })
	if gitAuthBind < 0 {
		t.Errorf("the worker's git in the rootfs binds %+v, want /run/hotbed/git-auth read-only", x.Config.ReadOnlyBinds)
	}
	for _, wantEnv := range []string{
		"HOTBED_GIT_CRED_SOCK=/run/hotbed/git-auth/git-cred.sock",
		"HOTBED_GIT_SSH_KEY=/run/hotbed/git-auth/ssh_id",
		"GIT_CONFIG_KEY_0=credential.useHttpPath",
		"PATH=/run/hotbed/git-auth/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	} {
		if !slices.Contains(x.Cmd.Env, wantEnv) {
			t.Errorf("the worker's git in the rootfs lacks %q: %v", wantEnv, x.Cmd.Env)
		}
	}
}
