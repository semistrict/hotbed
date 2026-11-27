package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

func newPrivateHTTPRepo(t *testing.T, repoPath, wantUser, wantPass string) (string, string) {
	t.Helper()
	src := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", src}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	runGit("init", "--quiet", "-b", "main")
	if err := os.WriteFile(filepath.Join(src, "lib.txt"), []byte("private\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "lib.txt")
	runGit("commit", "--quiet", "-m", "initial")

	bare := filepath.Join(t.TempDir(), "repo.git")
	if out, err := exec.Command("git", "clone", "--bare", "--quiet", src, bare).CombinedOutput(); err != nil {
		t.Fatalf("git clone --bare: %v: %s", err, out)
	}
	for _, kv := range [][2]string{
		{"uploadpack.allowFilter", "true"},
		{"uploadpack.allowAnySHA1InWant", "true"},
	} {
		if out, err := exec.Command("git", "-C", bare, "config", kv[0], kv[1]).CombinedOutput(); err != nil {
			t.Fatalf("git config %s: %v: %s", kv[0], err, out)
		}
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != wantUser || p != wantPass {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		var env []string
		if gp := r.Header.Get("Git-Protocol"); gp != "" {
			env = append(os.Environ(), "GIT_PROTOCOL="+gp)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == repoPath+"/info/refs" && r.URL.Query().Get("service") == "git-upload-pack":
			cmd := exec.CommandContext(r.Context(), "git", "upload-pack", "--stateless-rpc", "--advertise-refs", bare)
			cmd.Env = env
			out, err := cmd.Output()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			const hdr = "# service=git-upload-pack\n"
			fmt.Fprintf(w, "%04x%s0000", len(hdr)+4, hdr)
			_, _ = w.Write(out)
		case r.Method == http.MethodPost && r.URL.Path == repoPath+"/git-upload-pack":
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
			cmd := exec.CommandContext(r.Context(), "git", "upload-pack", "--stateless-rpc", bare)
			cmd.Env = env
			cmd.Stdin = body
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				http.Error(w, stderr.String(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
			_, _ = w.Write(stdout.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts.URL + repoPath, strings.TrimPrefix(ts.URL, "http://")
}

func TestCentralizedGitAuthCLIAndEndToEndMultiNode(t *testing.T) {
	t.Parallel()
	// asahimbp (arm64) and mini (amd64), neither with local git credentials:
	// a run reaches each by its arch.
	c := newMixedArchFleet(t)
	c.online()

	// Mock GitHub OAuth + REST + GraphQL endpoints for CLI auth login / auth api / worker `gh`.
	fakeGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/login/device/code":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code":      "dev_cli_1",
				"user_code":        "HOTB-7060",
				"verification_uri": "https://github.com/login/device",
				"expires_in":       600,
				"interval":         1,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/login/oauth/access_token":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":             "ghu_oauth_cli_token",
				"refresh_token":            "ghr_oauth_refresh",
				"expires_in":               28800,
				"refresh_token_expires_in": 15897600,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/user":
			if r.Header.Get("Authorization") != "Bearer ghu_oauth_cli_token" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"login": "semistrict"})
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			if r.Header.Get("Authorization") != "Bearer ghu_oauth_cli_token" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"viewer": map[string]string{"login": "semistrict"}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fakeGH.Close)

	// 1. Exercise `auth login-start` + `auth login --login-id` (two-step) AND interactive `auth login`.
	var challenge pb.GitHubLoginChallenge
	c.ok(&challenge, "auth", "login-start", "--name", "gh-step", "--oauth-url", fakeGH.URL, "--api-url", fakeGH.URL)
	if challenge.UserCode != "HOTB-7060" || challenge.LoginId == "" {
		t.Fatalf("auth login-start = %+v", &challenge)
	}
	var stepCred pb.GitCredential
	c.ok(&stepCred, "auth", "login", "--login-id", challenge.LoginId)
	if stepCred.Name != "gh-step" || stepCred.Username != "semistrict" || !stepCred.Refreshable {
		t.Fatalf("auth login --login-id = %+v", &stepCred)
	}
	c.ok(nil, "auth", "rm", "gh-step")

	// Interactive `auth login` runs StartGitHubLoginInteractive hook and prints the one-time code to stderr.
	code, out, errOut := c.hotbed("auth", "login", "--oauth-url", fakeGH.URL, "--api-url", fakeGH.URL)
	if code != 0 || !strings.Contains(errOut, "HOTB-7060") || !strings.Contains(out, "user=semistrict") {
		t.Fatalf("interactive auth login failed (code=%d): out=%q err=%q", code, out, errOut)
	}

	// 2. Exercise `auth api` (REST & GraphQL) directly via CLI.
	var apiResp pb.ProxyGitHubAPIResponse
	c.ok(&apiResp, "auth", "api", "/user")
	if apiResp.StatusCode != 200 || !strings.Contains(apiResp.Body, `"semistrict"`) {
		t.Fatalf("auth api /user = %+v", &apiResp)
	}
	var gqlResp pb.ProxyGitHubAPIResponse
	c.ok(&gqlResp, "auth", "api", "/graphql", "--body", `{"query":"query { viewer { login } }"}`)
	if gqlResp.StatusCode != 200 || !strings.Contains(gqlResp.Body, `"semistrict"`) {
		t.Fatalf("auth api /graphql = %+v", &gqlResp)
	}

	// 3. Private repo setup + hard failure before host credential is configured.
	repoURL, hostPort := newPrivateHTTPRepo(t, "/semistrict/hotbed.git", "x-access-token", "cluster-secret-token")
	sshURL := "ssh://git@" + hostPort + "/semistrict/hotbed.git"
	c.ok(nil, "project", "add", "hotbed", "--repo", repoURL)
	c.ok(nil, "repo", "add", "hotbed", repoURL, "-p", "hotbed")
	c.ok(nil, "session", "start", "auth-sess", "-p", "hotbed")

	_, err := c.fails("run", "-s", "auth-sess", "-r", "hotbed", "--arch", "arm64", "--", "true")
	if err.Code != api.CodeNotAuthorized || err.Retryable {
		t.Fatalf("run before auth set = %+v, want non-retryable NOT_AUTHORIZED", err)
	}

	// 4. `auth set` via stdin (--token-file -) so secrets never appear in argv.
	var cred pb.GitCredential
	c.okIn("cluster-secret-token\n", &cred, "auth", "set", "localgh", "--host", hostPort, "--repo", "semistrict/*", "--token-file", "-")
	if cred.Name != "localgh" || cred.Host != hostPort {
		t.Fatalf("auth set = %+v", &cred)
	}

	// 5. `auth ls` returns metadata only.
	var ls pb.ListGitCredentialsResponse
	c.ok(&ls, "auth", "ls", "--host", hostPort)
	if len(ls.Credentials) != 1 || ls.Credentials[0].Name != "localgh" {
		t.Fatalf("auth ls = %+v", &ls)
	}

	// 6. `auth token` returns token in --json and redacts it in default plain text.
	var tok pb.GitToken
	c.ok(&tok, "auth", "token", repoURL)
	if tok.Token != "cluster-secret-token" || tok.Username != "x-access-token" || tok.Repository != "semistrict/hotbed" {
		t.Fatalf("auth token = %+v", &tok)
	}
	code, plainOut, plainErr := c.hotbed("auth", "token", repoURL)
	if code != 0 {
		t.Fatalf("plain auth token failed (%d): %s", code, plainErr)
	}
	if strings.Contains(plainOut, "cluster-secret-token") {
		t.Fatalf("plain auth token printed raw secret to terminal: %q", plainOut)
	}

	// 7. End-to-end across BOTH worker nodes: HTTPS git, SSH proxy git, AND worker fake `gh` (REST + GraphQL)!
	for node, arch := range map[string]string{"asahimbp": "arm64", "mini": "amd64"} {
		code, out, stderr := c.hotbed("run", "-s", "auth-sess", "-r", "hotbed", "--arch", arch, "--", "git", "ls-remote", "origin", "HEAD")
		if code != 0 || !strings.Contains(out, "HEAD") {
			t.Fatalf("HTTPS git on %s failed (code=%d): out=%q err=%q", node, code, out, stderr)
		}
		code, out, stderr = c.hotbed("run", "-s", "auth-sess", "-r", "hotbed", "--arch", arch, "--", "git", "ls-remote", sshURL, "HEAD")
		if code != 0 || !strings.Contains(out, "HEAD") {
			t.Fatalf("SSH proxy git on %s failed (code=%d): out=%q err=%q", node, code, out, stderr)
		}
		code, out, stderr = c.hotbed("run", "-s", "auth-sess", "-r", "hotbed", "--arch", arch, "--", "gh", "auth", "status")
		if code != 0 || !strings.Contains(out, "semistrict") {
			t.Fatalf("worker gh auth status on %s failed (code=%d): out=%q err=%q", node, code, out, stderr)
		}
		code, out, stderr = c.hotbed("run", "-s", "auth-sess", "-r", "hotbed", "--arch", arch, "--", "gh", "api", "--hostname", "github.com", "/user")
		if code != 0 || !strings.Contains(out, `"semistrict"`) {
			t.Fatalf("worker gh api /user on %s failed (code=%d): out=%q err=%q", node, code, out, stderr)
		}
		code, out, stderr = c.hotbed("run", "-s", "auth-sess", "-r", "hotbed", "--arch", arch, "--", "gh", "api", "--hostname", "github.com", "graphql", "-f", "query=query { viewer { login } }")
		if code != 0 || !strings.Contains(out, `"semistrict"`) {
			t.Fatalf("worker gh api graphql on %s failed (code=%d): out=%q err=%q", node, code, out, stderr)
		}
	}

	// 8. `auth rm` removes credentials and immediately blocks new token mints.
	var del pb.DeleteGitCredentialResponse
	c.ok(&del, "auth", "rm", "localgh")
	if del.Name != "localgh" {
		t.Fatalf("auth rm = %+v", &del)
	}
	_, err = c.fails("auth", "token", repoURL)
	if err.Code != api.CodeNotFound {
		t.Fatalf("auth token after rm = %+v, want NOT_FOUND", err)
	}
	c.ok(nil, "auth", "rm", "github")
}
