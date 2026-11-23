package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/statedir"
	"github.com/semistrict/hotbed/internal/tailnet"
	"github.com/semistrict/hotbed/internal/tailnet/tailnettest"
)

func newGitAuthTestClient(t *testing.T, clock *time.Time) (hotbedv1connect.GitAuthServiceClient, *Server) {
	t.Helper()
	var clockMu sync.Mutex
	ctrlSelf := tailnet.Self{Peer: tailnet.Peer{NodeID: "nCtrl", Hostname: "ubu1", Login: "ramon@"}}
	caller := tailnet.Peer{NodeID: "nLaptop", Hostname: "laptop", Login: "ramon@"}
	tn := tailnettest.New(ctrlSelf)
	store, err := OpenStore(t.Context(), filepath.Join(t.TempDir(), "hotbed.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	srv := NewServer(ServerConfig{
		Store:      store,
		Identifier: tn,
		Cluster:    statedir.ControllerConfig{ClusterID: "c1", Owner: ctrlSelf.Login},
		Self:       []netip.Addr{netip.MustParseAddr("127.0.0.1")},
		Now: func() time.Time {
			clockMu.Lock()
			defer clockMu.Unlock()
			return *clock
		},
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	conn, err := api.Dial(strings.TrimPrefix(ts.URL, "http://"), api.DefaultPort, tn.Client(caller))
	if err != nil {
		t.Fatal(err)
	}
	return hotbedv1connect.NewGitAuthServiceClient(conn), srv
}

func TestGitAuthTokenLifecycleAndSpecificity(t *testing.T) {
	now := time.Date(2026, 11, 22, 14, 0, 0, 0, time.UTC)
	cl, _ := newGitAuthTestClient(t, &now)
	ctx := t.Context()

	// No credentials configured -> clear NOT_FOUND error (no silent fallback).
	if _, err := cl.MintGitToken(ctx, &pb.MintGitTokenRequest{Repo: "https://github.com/semistrict/hotbed.git"}); api.CodeOf(err) != api.CodeNotFound {
		t.Fatalf("MintGitToken with no credentials = %v, want NOT_FOUND", err)
	}

	// Host-wide, org wildcard, and exact repo credentials.
	if _, err := cl.PutGitCredential(ctx, &pb.PutGitCredentialRequest{
		Name: "hostwide", Host: "github.com", Token: "tok-host", Username: "ramon",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.PutGitCredential(ctx, &pb.PutGitCredentialRequest{
		Name: "org", Host: "github.com", Repos: []string{"semistrict/*"}, Token: "tok-org",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.PutGitCredential(ctx, &pb.PutGitCredentialRequest{
		Name: "exact", Host: "github.com", Repos: []string{"semistrict/hotbed"}, Token: "tok-exact",
	}); err != nil {
		t.Fatal(err)
	}

	// List returns metadata only; secrets never appear.
	list, err := cl.ListGitCredentials(ctx, &pb.ListGitCredentialsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Credentials) != 3 {
		t.Fatalf("len(credentials) = %d, want 3", len(list.Credentials))
	}
	raw, _ := json.Marshal(list)
	for _, secret := range []string{"tok-host", "tok-org", "tok-exact"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("ListGitCredentials leaked secret %q: %s", secret, raw)
		}
	}

	// Exact match wins over org wildcard, which wins over host-wide.
	cases := []struct {
		repo     string
		wantCred string
		wantTok  string
	}{
		{"https://github.com/semistrict/hotbed.git", "exact", "tok-exact"},
		{"git@github.com:Semistrict/Hotbed.git", "exact", "tok-exact"},
		{"https://github.com/semistrict/cladefs.git", "org", "tok-org"},
		{"github.com/otherorg/private", "hostwide", "tok-host"},
	}
	for _, tc := range cases {
		tok, err := cl.MintGitToken(ctx, &pb.MintGitTokenRequest{Repo: tc.repo})
		if err != nil {
			t.Fatalf("MintGitToken(%q): %v", tc.repo, err)
		}
		if tok.Credential != tc.wantCred || tok.Token != tc.wantTok || tok.Username != "x-access-token" {
			t.Errorf("MintGitToken(%q) = cred=%q tok=%q user=%q, want %q / %q / x-access-token",
				tc.repo, tok.Credential, tok.Token, tok.Username, tc.wantCred, tc.wantTok)
		}
		if tok.ExpiresAt != now.Add(gitTokenLeaseTTL).UnixMilli() {
			t.Errorf("MintGitToken(%q) expires_at = %d, want %d", tc.repo, tok.ExpiresAt, now.Add(gitTokenLeaseTTL).UnixMilli())
		}
	}

	// Deleting hostwide means otherorg/private is immediately refused.
	if _, err := cl.DeleteGitCredential(ctx, &pb.DeleteGitCredentialRequest{Name: "hostwide"}); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.MintGitToken(ctx, &pb.MintGitTokenRequest{Repo: "https://github.com/otherorg/private.git"}); api.CodeOf(err) != api.CodeNotFound {
		t.Fatalf("MintGitToken after delete = %v, want NOT_FOUND", err)
	}
}

func TestGitAuthOAuthDeviceFlowAndAutoRefresh(t *testing.T) {
	now := time.Date(2026, 11, 22, 14, 0, 0, 0, time.UTC)
	cl, _ := newGitAuthTestClient(t, &now)
	ctx := t.Context()

	var pollCalls, refreshCalls atomic.Int32
	fakeGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/login/device/code":
			if err := r.ParseForm(); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if r.Form.Get("client_id") == "" || r.Form.Get("client_secret") != "" {
				t.Errorf("device/code form = %v, want public client_id with no client_secret", r.Form)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code":      "dev_code_123",
				"user_code":        "ABCD-1234",
				"verification_uri": "https://github.com/login/device",
				"expires_in":       900,
				"interval":         1,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/login/oauth/access_token":
			if err := r.ParseForm(); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if r.Form.Get("client_secret") != "" {
				t.Errorf("access_token unexpectedly sent client_secret")
			}
			switch r.Form.Get("grant_type") {
			case "urn:ietf:params:oauth:grant-type:device_code":
				if pollCalls.Add(1) == 1 {
					_ = json.NewEncoder(w).Encode(map[string]any{"error": "authorization_pending"})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"access_token":             "ghu_initial_token",
					"refresh_token":            "ghr_refresh_1",
					"expires_in":               28800,
					"refresh_token_expires_in": 15897600,
				})
			case "refresh_token":
				n := refreshCalls.Add(1)
				if r.Form.Get("refresh_token") != "ghr_refresh_1" {
					t.Errorf("refresh_token = %q, want ghr_refresh_1", r.Form.Get("refresh_token"))
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"access_token":             "ghu_refreshed_" + string(rune('0'+n)),
					"refresh_token":            "ghr_refresh_2",
					"expires_in":               28800,
					"refresh_token_expires_in": 15897600,
				})
			default:
				http.Error(w, "unknown grant_type", http.StatusBadRequest)
			}
		case r.Method == http.MethodGet && r.URL.Path == "/user":
			if r.Header.Get("Authorization") != "Bearer ghu_initial_token" {
				http.Error(w, "bad token", http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"login": "semistrict"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fakeGH.Close)

	ch, err := cl.StartGitHubLogin(ctx, &pb.StartGitHubLoginRequest{
		Name:     "github",
		Host:     "github.com",
		OauthUrl: fakeGH.URL,
		ApiUrl:   fakeGH.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ch.UserCode != "ABCD-1234" || ch.VerificationUri != "https://github.com/login/device" || ch.LoginId == "" {
		t.Fatalf("unexpected challenge: %+v", ch)
	}

	cred, err := cl.CompleteGitHubLogin(ctx, &pb.CompleteGitHubLoginRequest{LoginId: ch.LoginId})
	if err != nil {
		t.Fatal(err)
	}
	if cred.Username != "semistrict" || !cred.Refreshable || cred.ExpiresAt == 0 {
		t.Fatalf("unexpected credential after OAuth login: %+v", cred)
	}
	if pollCalls.Load() != 2 {
		t.Errorf("pollCalls = %d, want 2 (1 pending + 1 success)", pollCalls.Load())
	}

	// Token returned without refreshing while well before expiry.
	tok1, err := cl.MintGitToken(ctx, &pb.MintGitTokenRequest{Repo: "https://github.com/semistrict/hotbed.git"})
	if err != nil {
		t.Fatal(err)
	}
	if tok1.Token != "ghu_initial_token" || refreshCalls.Load() != 0 {
		t.Fatalf("tok1 = %q, refreshCalls = %d", tok1.Token, refreshCalls.Load())
	}

	// Advance clock to within 5 minutes of the 8h expiry and fire concurrent mints:
	// refresh must serialize so GitHub only sees ONE refresh_token exchange.
	now = now.Add(8*time.Hour - 2*time.Minute)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	tokens := make(chan string, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := cl.MintGitToken(ctx, &pb.MintGitTokenRequest{Repo: "https://github.com/semistrict/hotbed.git"})
			if err != nil {
				errs <- err
				return
			}
			tokens <- tok.Token
		}()
	}
	wg.Wait()
	close(errs)
	close(tokens)
	for err := range errs {
		t.Fatalf("concurrent MintGitToken failed: %v", err)
	}
	for tok := range tokens {
		if tok != "ghu_refreshed_1" {
			t.Errorf("got token %q, want ghu_refreshed_1", tok)
		}
	}
	if refreshCalls.Load() != 1 {
		t.Errorf("refreshCalls = %d, want 1 (serialized)", refreshCalls.Load())
	}
}

func TestGitAuthProxyGitHubAPI(t *testing.T) {
	now := time.Date(2026, 11, 22, 14, 0, 0, 0, time.UTC)
	cl, _ := newGitAuthTestClient(t, &now)
	ctx := t.Context()

	var gotAuth, gotCookie, gotCustom, gotPath, gotMethod, gotBody string
	fakeAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCookie = r.Header.Get("Cookie")
		gotCustom = r.Header.Get("X-Custom")
		gotPath = r.URL.RequestURI()
		gotMethod = r.Method
		var buf strings.Builder
		_, _ = buf.WriteString("")
		b := make([]byte, 4096)
		n, _ := r.Body.Read(b)
		gotBody = string(b[:n])
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "session=secret-do-not-leak")
		w.Header().Set("X-RateLimit-Remaining", "4999")
		if r.URL.Path == "/graphql" {
			_, _ = w.Write([]byte(`{"data":{"viewer":{"login":"semistrict"}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"full_name":"semistrict/hotbed","private":true}`))
	}))
	t.Cleanup(fakeAPI.Close)

	if _, err := cl.PutGitCredential(ctx, &pb.PutGitCredentialRequest{
		Name: "github", Host: "github.com", Token: "gho_proxy_tok", ApiUrl: fakeAPI.URL,
	}); err != nil {
		t.Fatal(err)
	}

	// REST GET with spoofed Authorization & Cookie headers that must be scrubbed.
	resp, err := cl.ProxyGitHubAPI(ctx, &pb.ProxyGitHubAPIRequest{
		Path: "/repos/semistrict/hotbed?per_page=10",
		Headers: []string{
			"Authorization: Bearer spoofed",
			"Cookie: evil=1",
			"X-Custom: ok",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || !strings.Contains(resp.Body, `"semistrict/hotbed"`) {
		t.Fatalf("unexpected REST response: %+v", resp)
	}
	if gotAuth != "Bearer gho_proxy_tok" || gotCookie != "" || gotCustom != "ok" || gotMethod != "GET" || gotPath != "/repos/semistrict/hotbed?per_page=10" {
		t.Fatalf("upstream saw auth=%q cookie=%q custom=%q method=%q path=%q", gotAuth, gotCookie, gotCustom, gotMethod, gotPath)
	}
	for _, h := range resp.Headers {
		if strings.HasPrefix(strings.ToLower(h), "set-cookie:") {
			t.Errorf("ProxyGitHubAPI leaked Set-Cookie header: %q", h)
		}
	}

	// GraphQL POST (method defaults to POST when body is set).
	gql, err := cl.ProxyGitHubAPI(ctx, &pb.ProxyGitHubAPIRequest{
		Path: "/graphql",
		Body: `{"query":"query { viewer { login } }"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if gql.StatusCode != 200 || gotMethod != "POST" || !strings.Contains(gotBody, "viewer") || !strings.Contains(gql.Body, "semistrict") {
		t.Fatalf("unexpected GraphQL proxy result: resp=%+v method=%q body=%q", gql, gotMethod, gotBody)
	}

	// Invalid paths (SSRF / scheme injection) must be rejected.
	for _, badPath := range []string{"user", "//evil.com/x", "https://evil.com/x", "/foo://bar"} {
		if _, err := cl.ProxyGitHubAPI(ctx, &pb.ProxyGitHubAPIRequest{Path: badPath}); api.CodeOf(err) != api.CodeInvalidArgs {
			t.Errorf("ProxyGitHubAPI(%q) = %v, want INVALID_ARGS", badPath, err)
		}
	}
}

func TestGitAuthValidationAndRepoParsing(t *testing.T) {
	now := time.Now()
	cl, _ := newGitAuthTestClient(t, &now)
	ctx := t.Context()

	badReqs := []*pb.PutGitCredentialRequest{
		{Name: "BadName", Token: "tok"},
		{Name: "empty"},
		{Name: "newline", Token: "tok\ninjected"},
		{Name: "badrepo", Token: "tok", Repos: []string{"noslash"}},
		{Name: "badhost", Host: "https://github.com", Token: "tok"},
	}
	for _, req := range badReqs {
		if _, err := cl.PutGitCredential(ctx, req); api.CodeOf(err) != api.CodeInvalidArgs {
			t.Errorf("PutGitCredential(%q) = %v, want INVALID_ARGS", req.Name, err)
		}
	}

	for _, tc := range []struct {
		raw        string
		h, o, repo string
	}{
		{"https://github.com/semistrict/hotbed.git", "github.com", "semistrict", "hotbed"},
		{"https://github.com:443/semistrict/hotbed", "github.com", "semistrict", "hotbed"},
		{"http://127.0.0.1:54321/semistrict/hotbed.git", "127.0.0.1:54321", "semistrict", "hotbed"},
		{"127.0.0.1:54321/semistrict/hotbed.git", "127.0.0.1:54321", "semistrict", "hotbed"},
		{"git@github.com:semistrict/hotbed.git", "github.com", "semistrict", "hotbed"},
		{"ssh://git@github.com/semistrict/hotbed.git", "github.com", "semistrict", "hotbed"},
		{"github.com/semistrict/hotbed", "github.com", "semistrict", "hotbed"},
	} {
		h, o, r, err := parseGitRepo(tc.raw)
		if err != nil || h != tc.h || o != tc.o || r != tc.repo {
			t.Errorf("parseGitRepo(%q) = (%q, %q, %q, %v), want (%q, %q, %q, nil)", tc.raw, h, o, r, err, tc.h, tc.o, tc.repo)
		}
	}
}
