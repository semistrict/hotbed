package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/cliauth"
)

func TestLoginWhoamiOrgAndLogoutCommands(t *testing.T) {
	hotbedDir := t.TempDir()
	origDir := defaultHotbedDir
	defaultHotbedDir = func() (string, error) { return hotbedDir, nil }
	t.Cleanup(func() { defaultHotbedDir = origDir })
	where := newTestHere(t, nil)

	activeOrg := cliauth.OrganizationSummary{ID: "org_alpha", Name: "Alpha", Role: "admin"}
	orgs := []cliauth.OrganizationSummary{
		activeOrg,
		{ID: "org_beta", Name: "Beta", Role: "member"},
	}

	var opened []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/auth/cli/device/start" && r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"deviceCode":              "dev_cli_1",
				"userCode":                "WXYZ-1234",
				"verificationUri":         "https://auth.example/device",
				"verificationUriComplete": "https://auth.example/device?user_code=WXYZ-1234",
				"expiresIn":               300,
				"interval":                1,
			})
		case r.URL.Path == "/auth/cli/device/poll" && r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(cliauth.TokenResponse{
				Status:        "complete",
				AccessToken:   "jwt_alpha",
				RefreshToken:  "rt_1",
				ExpiresAt:     time.Now().Add(10 * time.Minute).UnixMilli(),
				User:          cliauth.UserInfo{ID: "user_1", Email: "ramon@alpha.test", Name: "Ramon"},
				Organization:  &activeOrg,
				Organizations: orgs,
			})
		case r.URL.Path == "/auth/me" && r.Method == http.MethodGet:
			if r.Header.Get("Authorization") == "" {
				http.Error(w, `{"code":"NOT_AUTHORIZED","message":"auth required"}`, 401)
				return
			}
			_ = json.NewEncoder(w).Encode(whoamiResponse{
				Authenticated: true,
				AuthMethod:    "workos-jwt",
				WorkOSEnabled: true,
				User:          cliauth.UserInfo{ID: "user_1", Email: "ramon@alpha.test", Name: "Ramon"},
				Organization:  &activeOrg,
				Organizations: orgs,
			})
		case r.URL.Path == "/auth/cli/refresh" && r.Method == http.MethodPost:
			var body struct {
				OrganizationID string `json:"organizationId"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			for _, o := range orgs {
				if o.ID == body.OrganizationID {
					activeOrg = o
					break
				}
			}
			_ = json.NewEncoder(w).Encode(cliauth.TokenResponse{
				AccessToken:   "jwt_" + activeOrg.ID,
				RefreshToken:  "rt_2",
				ExpiresAt:     time.Now().Add(10 * time.Minute).UnixMilli(),
				User:          cliauth.UserInfo{ID: "user_1", Email: "ramon@alpha.test", Name: "Ramon"},
				Organization:  &activeOrg,
				Organizations: orgs,
			})
		case r.URL.Path == "/auth/org/invite" && r.Method == http.MethodPost:
			var body struct {
				Email string `json:"email"`
				Role  string `json:"role"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"invitation": map[string]any{"id": "inv_1", "email": body.Email, "role": body.Role, "state": "pending"},
			})
		case r.URL.Path == "/admin/join-tokens" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "jt_1", "token": "hbj_org_beta.secret", "orgId": activeOrg.ID,
			})
		case r.URL.Path == "/admin/api-tokens" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "at_1", "token": "hbu_org_beta.secret", "name": "ci", "orgId": activeOrg.ID,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	runCLI := func(args ...string) output {
		t.Helper()
		var stdout, stderr bytes.Buffer
		d := deps{
			here:       where.here(),
			http:       srv.Client(),
			stream:     srv.Client(),
			controller: at(srv.URL),
			stdout:     &stdout,
			stderr:     &stderr,
			openURL: func(_ context.Context, target string) error {
				opened = append(opened, target)
				return nil
			},
		}
		code := run(t.Context(), append([]string{"--json"}, args...), d)
		if code != exitOK {
			t.Fatalf("hotbed %v failed (code=%d): stdout=%s stderr=%s", args, code, stdout.String(), stderr.String())
		}
		var env output
		if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
			t.Fatalf("decode output for %v: %v (%s)", args, err, stdout.String())
		}
		return env
	}

	// 1. hotbed login (device-code flow, no localhost callback)
	loginOut := runCLI("login", srv.URL)
	var lr loginResult
	if err := json.Unmarshal(loginOut.Data, &lr); err != nil || lr.Organization == nil || lr.Organization.ID != "org_alpha" {
		t.Fatalf("unexpected login result: %s", loginOut.Data)
	}
	if len(opened) != 1 || opened[0] != "https://auth.example/device?user_code=WXYZ-1234" {
		t.Fatalf("opened URLs = %v, want device verification URL", opened)
	}

	// 2. hotbed whoami
	whoOut := runCLI("whoami")
	var wr whoamiResponse
	if err := json.Unmarshal(whoOut.Data, &wr); err != nil || wr.User.Email != "ramon@alpha.test" {
		t.Fatalf("unexpected whoami result: %s", whoOut.Data)
	}

	// 3. hotbed org switch Beta (by name!)
	swOut := runCLI("org", "switch", "Beta")
	var sr orgSwitchResult
	if err := json.Unmarshal(swOut.Data, &sr); err != nil || sr.Organization.ID != "org_beta" {
		t.Fatalf("unexpected org switch result: %s", swOut.Data)
	}

	// 4. hotbed org invite & join-token & api-token
	runCLI("org", "invite", "bob@beta.test", "--role", "member")
	runCLI("org", "join-token", "create", "--label", "build-box")
	runCLI("org", "api-token", "create", "ci")

	// 5. hotbed logout
	runCLI("logout", srv.URL)
	st, err := cliauth.Load(hotbedDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(st.Controllers) != 0 {
		t.Fatalf("expected empty auth store after logout, got %+v", st.Controllers)
	}
}

func TestOrgSwitchAndCreateUnderRefreshTokenRotationAndConcurrency(t *testing.T) {
	hotbedDir := t.TempDir()
	origDir := defaultHotbedDir
	defaultHotbedDir = func() (string, error) { return hotbedDir, nil }
	t.Cleanup(func() { defaultHotbedDir = origDir })
	where := newTestHere(t, nil)

	var mu sync.Mutex
	activeRT := map[string]bool{"rt_1": true}
	rtSeq := 1
	orgs := []cliauth.OrganizationSummary{
		{ID: "org_alpha", Name: "Alpha", Role: "admin"},
		{ID: "org_beta", Name: "Beta", Role: "member"},
	}

	rotateRT := func(incoming string) (string, bool) {
		mu.Lock()
		defer mu.Unlock()
		if !activeRT[incoming] {
			return "", false
		}
		delete(activeRT, incoming)
		rtSeq++
		next := "rt_" + string(rune('0'+rtSeq))
		activeRT[next] = true
		return next, true
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/auth/cli/refresh" && r.Method == http.MethodPost:
			var body struct {
				RefreshToken   string `json:"refreshToken"`
				OrganizationID string `json:"organizationId"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			nextRT, ok := rotateRT(body.RefreshToken)
			if !ok {
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]string{
					"code":    "NOT_AUTHORIZED",
					"message": "invalid_grant: refresh token " + body.RefreshToken + " already used",
				})
				return
			}
			org := orgs[0]
			for _, o := range orgs {
				if o.ID == body.OrganizationID {
					org = o
					break
				}
			}
			_ = json.NewEncoder(w).Encode(cliauth.TokenResponse{
				AccessToken:   "jwt_fresh_" + org.ID,
				RefreshToken:  nextRT,
				ExpiresAt:     time.Now().Add(10 * time.Minute).UnixMilli(),
				User:          cliauth.UserInfo{ID: "user_1", Email: "ramon@alpha.test", Name: "Ramon"},
				Organization:  &org,
				Organizations: orgs,
			})
		case r.URL.Path == "/auth/me" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(whoamiResponse{
				Authenticated: true,
				AuthMethod:    "workos-jwt",
				WorkOSEnabled: true,
				User:          cliauth.UserInfo{ID: "user_1", Email: "ramon@alpha.test", Name: "Ramon"},
				Organization:  &orgs[0],
				Organizations: orgs,
			})
		case r.URL.Path == "/auth/org/create" && r.Method == http.MethodPost:
			var body struct {
				Name         string `json:"name"`
				RefreshToken string `json:"refreshToken"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			nextRT, ok := rotateRT(body.RefreshToken)
			if !ok {
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]string{
					"code":    "NOT_AUTHORIZED",
					"message": "invalid_grant: refresh token " + body.RefreshToken + " already used",
				})
				return
			}
			newOrg := cliauth.OrganizationSummary{ID: "org_gamma", Name: body.Name, Role: "admin"}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"organization": newOrg,
				"accessToken":  "jwt_gamma",
				"refreshToken": nextRT,
				"expiresAt":    time.Now().Add(10 * time.Minute).UnixMilli(),
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	runCmd := func(args ...string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		d := deps{
			here:       where.here(),
			http:       srv.Client(),
			stream:     srv.Client(),
			controller: at(srv.URL),
			stdout:     &stdout,
			stderr:     &stderr,
		}
		code := run(t.Context(), append([]string{"--json"}, args...), d)
		return code, stdout.String(), stderr.String()
	}

	// 1. org switch by name with near-expiry JWT uses post-refresh token
	if err := cliauth.SaveCredentials(hotbedDir, srv.URL, cliauth.Credentials{
		AccessToken:    "jwt_expiring",
		RefreshToken:   "rt_1",
		ExpiresAt:      time.Now().Add(10 * time.Second).UnixMilli(),
		OrganizationID: "org_alpha",
	}, true); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}
	if code, stdout, stderr := runCmd("org", "switch", "Beta"); code != exitOK {
		t.Fatalf("org switch Beta failed (code=%d): stdout=%s stderr=%s", code, stdout, stderr)
	}

	// 2. org create with near-expiry JWT uses post-refresh token under lock
	mu.Lock()
	activeRT = map[string]bool{"rt_create_1": true}
	mu.Unlock()
	if err := cliauth.SaveCredentials(hotbedDir, srv.URL, cliauth.Credentials{
		AccessToken:    "jwt_expiring",
		RefreshToken:   "rt_create_1",
		ExpiresAt:      time.Now().Add(10 * time.Second).UnixMilli(),
		OrganizationID: "org_alpha",
	}, true); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}
	if code, stdout, stderr := runCmd("org", "create", "Gamma"); code != exitOK {
		t.Fatalf("org create Gamma failed (code=%d): stdout=%s stderr=%s", code, stdout, stderr)
	}

	// 3. concurrent org switch + ResolveToken serialize on auth.lock without double-spending refresh token
	mu.Lock()
	activeRT = map[string]bool{"rt_conc_1": true}
	mu.Unlock()
	if err := cliauth.SaveCredentials(hotbedDir, srv.URL, cliauth.Credentials{
		AccessToken:    "jwt_expiring",
		RefreshToken:   "rt_conc_1",
		ExpiresAt:      time.Now().Add(10 * time.Second).UnixMilli(),
		OrganizationID: "org_alpha",
	}, true); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}
	var wg sync.WaitGroup
	var switchCode int
	var switchOut string
	var resolveErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		switchCode, switchOut, _ = runCmd("org", "switch", "org_beta")
	}()
	go func() {
		defer wg.Done()
		_, resolveErr = cliauth.ResolveToken(t.Context(), hotbedDir, srv.URL, "", srv.Client())
	}()
	wg.Wait()
	if switchCode != exitOK || resolveErr != nil {
		t.Fatalf("concurrent org switch + ResolveToken failed: switchCode=%d switchOut=%s resolveErr=%v", switchCode, switchOut, resolveErr)
	}
}

func TestControllerAuthTokenFailsClosedOnHotbedOrgError(t *testing.T) {
	hotbedDir := t.TempDir()
	origDir := defaultHotbedDir
	defaultHotbedDir = func() (string, error) { return hotbedDir, nil }
	t.Cleanup(func() { defaultHotbedDir = origDir })
	where := newTestHere(t, map[string]string{"HOTBED_ORG": "org_unauthorized"})

	controllerURL := "https://127.0.0.1:1"
	if err := os.WriteFile(filepath.Join(hotbedDir, "token"), []byte("legacy_owner_token_wrong_org\n"), 0o600); err != nil {
		t.Fatalf("write legacy token: %v", err)
	}
	if err := cliauth.SaveCredentials(hotbedDir, controllerURL, cliauth.Credentials{
		AccessToken:    "jwt_acme",
		RefreshToken:   "rt_acme",
		ExpiresAt:      time.Now().Add(10 * time.Minute).UnixMilli(),
		OrganizationID: "org_acme",
	}, true); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}

	if got := controllerAuthToken(where.getenv, controllerURL); got != "" {
		t.Fatalf("controllerAuthToken fell back to %q on HOTBED_ORG refresh failure; want empty", got)
	}
	if _, err := resolveControllerAuthToken(t.Context(), where.getenv, controllerURL, nil); err == nil {
		t.Fatalf("resolveControllerAuthToken returned nil error on failed HOTBED_ORG refresh; want error")
	}
}

func TestLoginSkipsBrowserOpenOverSSH(t *testing.T) {
	hotbedDir := t.TempDir()
	origDir := defaultHotbedDir
	defaultHotbedDir = func() (string, error) { return hotbedDir, nil }
	t.Cleanup(func() { defaultHotbedDir = origDir })
	where := newTestHere(t, map[string]string{"SSH_CONNECTION": "10.0.0.2 54321 10.0.0.1 22"})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/cli/device/start":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"deviceCode":              "dev_ssh_1",
				"userCode":                "SSH1-CODE",
				"verificationUri":         "https://auth.example/device",
				"verificationUriComplete": "https://auth.example/device?user_code=SSH1-CODE",
				"expiresIn":               300,
				"interval":                1,
			})
		case "/auth/cli/device/poll":
			_ = json.NewEncoder(w).Encode(cliauth.TokenResponse{
				Status:       "complete",
				AccessToken:  "jwt_ssh",
				RefreshToken: "rt_ssh",
				ExpiresAt:    time.Now().Add(10 * time.Minute).UnixMilli(),
				User:         cliauth.UserInfo{ID: "user_1", Email: "ramon@alpha.test", Name: "Ramon"},
				Organization: &cliauth.OrganizationSummary{ID: "org_alpha", Name: "Alpha", Role: "admin"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	opened := false
	d := deps{
		here:       where.here(),
		http:       srv.Client(),
		stream:     srv.Client(),
		controller: at(srv.URL),
		stdout:     &stdout,
		stderr:     &stderr,
		openURL: func(_ context.Context, _ string) error {
			opened = true
			return nil
		},
	}
	if code := run(t.Context(), []string{"--json", "login", srv.URL}, d); code != exitOK {
		t.Fatalf("hotbed login over SSH failed (code=%d): stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if opened {
		t.Fatalf("hotbed login over SSH unexpectedly tried to open a local browser")
	}
	if !strings.Contains(stderr.String(), "SSH1-CODE") || !strings.Contains(stderr.String(), "https://auth.example/device?user_code=SSH1-CODE") {
		t.Fatalf("stderr missing code or URL: %s", stderr.String())
	}
}
