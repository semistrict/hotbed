package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/cliauth"
)

func TestLoginWhoamiOrgAndLogoutCommands(t *testing.T) {
	hotbedDir := t.TempDir()
	origDir := defaultHotbedDir
	defaultHotbedDir = func() (string, error) { return hotbedDir, nil }
	t.Cleanup(func() { defaultHotbedDir = origDir })
	t.Setenv("HOTBED_TOKEN", "")
	t.Setenv("HOTBED_ORG", "")

	activeOrg := cliauth.OrganizationSummary{ID: "org_alpha", Name: "Alpha", Role: "admin"}
	orgs := []cliauth.OrganizationSummary{
		activeOrg,
		{ID: "org_beta", Name: "Beta", Role: "member"},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/auth/cli/authorize" && r.Method == http.MethodGet:
			q := r.URL.Query()
			dest, _ := url.Parse(q.Get("redirect_uri"))
			dq := dest.Query()
			dq.Set("code", "cli_code_1")
			dq.Set("state", q.Get("state"))
			dest.RawQuery = dq.Encode()
			http.Redirect(w, r, dest.String(), http.StatusFound)
		case r.URL.Path == "/auth/cli/exchange" && r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(cliauth.TokenResponse{
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
			http:       srv.Client(),
			stream:     srv.Client(),
			controller: at(srv.URL),
			stdout:     &stdout,
			stderr:     &stderr,
			openURL: func(ctx context.Context, target string) error {
				go func() {
					req, _ := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
					resp, err := srv.Client().Do(req)
					if err == nil {
						_, _ = io.Copy(io.Discard, resp.Body)
						resp.Body.Close()
					}
				}()
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

	// 1. hotbed login
	loginOut := runCLI("login", srv.URL)
	var lr loginResult
	if err := json.Unmarshal(loginOut.Data, &lr); err != nil || lr.Organization == nil || lr.Organization.ID != "org_alpha" {
		t.Fatalf("unexpected login result: %s", loginOut.Data)
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
