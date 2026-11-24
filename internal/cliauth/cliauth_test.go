package cliauth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

func TestSaveLoadAndAutoRefreshToken(t *testing.T) {
	dir := t.TempDir()
	var refreshCalls atomic.Int32
	var lastOrg string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/cli/refresh" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		refreshCalls.Add(1)
		var body struct {
			RefreshToken   string `json:"refreshToken"`
			OrganizationID string `json:"organizationId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		lastOrg = body.OrganizationID
		_ = json.NewEncoder(w).Encode(TokenResponse{
			AccessToken:  "jwt_fresh_" + body.OrganizationID,
			RefreshToken: "rt_rotated",
			ExpiresAt:    time.Now().Add(10 * time.Minute).UnixMilli(),
			User:         UserInfo{ID: "user_1", Email: "alice@acme.test", Name: "Alice"},
			Organization: &OrganizationSummary{ID: body.OrganizationID, Name: "Acme", Role: "admin"},
		})
	}))
	defer srv.Close()

	// Save an already-expiring token.
	err := SaveCredentials(dir, srv.URL, Credentials{
		AccessToken:      "jwt_old",
		RefreshToken:     "rt_old",
		ExpiresAt:        time.Now().Add(10 * time.Second).UnixMilli(),
		UserID:           "user_1",
		Email:            "alice@acme.test",
		OrganizationID:   "org_acme",
		OrganizationName: "Acme",
		Role:             "admin",
	}, true)
	if err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}
	if got := DefaultController(dir); got != srv.URL {
		t.Fatalf("DefaultController = %q, want %q", got, srv.URL)
	}

	// ResolveToken automatically refreshes and persists the new token.
	tok, err := ResolveToken(t.Context(), dir, srv.URL, "", srv.Client())
	if err != nil {
		t.Fatalf("ResolveToken: %v", err)
	}
	if tok != "jwt_fresh_org_acme" {
		t.Fatalf("token = %q, want jwt_fresh_org_acme", tok)
	}
	if refreshCalls.Load() != 1 || lastOrg != "org_acme" {
		t.Fatalf("refreshCalls=%d lastOrg=%q", refreshCalls.Load(), lastOrg)
	}

	// Second call reuses the fresh cached token without hitting the server again.
	tok2, err := ResolveToken(t.Context(), dir, srv.URL, "", srv.Client())
	if err != nil {
		t.Fatalf("ResolveToken(2): %v", err)
	}
	if tok2 != "jwt_fresh_org_acme" || refreshCalls.Load() != 1 {
		t.Fatalf("unexpected refresh on fresh token: tok=%q calls=%d", tok2, refreshCalls.Load())
	}

	// Overriding targetOrg refreshes into that org without overwriting default org in auth.json.
	tok3, err := ResolveToken(t.Context(), dir, srv.URL, "org_beta", srv.Client())
	if err != nil {
		t.Fatalf("ResolveToken(org_beta): %v", err)
	}
	if tok3 != "jwt_fresh_org_beta" || lastOrg != "org_beta" {
		t.Fatalf("tok3=%q lastOrg=%q", tok3, lastOrg)
	}
	st, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if st.Controllers[srv.URL].OrganizationID != "org_acme" {
		t.Fatalf("stored org overwritten: got %q, want org_acme", st.Controllers[srv.URL].OrganizationID)
	}
}

func TestLoginBrowserPKCEFlow(t *testing.T) {
	var seenChallenge string
	var seenRedirect string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/cli/authorize":
			q := r.URL.Query()
			seenChallenge = q.Get("code_challenge")
			seenRedirect = q.Get("redirect_uri")
			dest, _ := url.Parse(seenRedirect)
			dq := dest.Query()
			dq.Set("code", "handoff_code_123")
			dq.Set("state", q.Get("state"))
			dest.RawQuery = dq.Encode()
			http.Redirect(w, r, dest.String(), http.StatusFound)
		case "/auth/cli/exchange":
			var body struct {
				Code         string `json:"code"`
				CodeVerifier string `json:"codeVerifier"`
				RedirectURI  string `json:"redirectUri"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			if body.Code != "handoff_code_123" || body.RedirectURI != seenRedirect || pkceChallenge(body.CodeVerifier) != seenChallenge {
				http.Error(w, `{"code":"NOT_AUTHORIZED","message":"pkce mismatch"}`, 401)
				return
			}
			_ = json.NewEncoder(w).Encode(TokenResponse{
				AccessToken:  "jwt_browser_ok",
				RefreshToken: "rt_browser_ok",
				ExpiresAt:    time.Now().Add(5 * time.Minute).UnixMilli(),
				User:         UserInfo{ID: "user_1", Email: "ramon@echophase.test", Name: "Ramon"},
				Organization: &OrganizationSummary{ID: "org_echo", Name: "Echophase", Role: "admin"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// Simulate the user's browser following the authorize URL and hitting the loopback callback.
	openURL := func(ctx context.Context, target string) error {
		go func() {
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
			resp, err := srv.Client().Do(req)
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
		return nil
	}

	resp, err := LoginBrowser(t.Context(), srv.URL, "org_echo", "GoogleOAuth", openURL, io.Discard, srv.Client())
	if err != nil {
		t.Fatalf("LoginBrowser: %v", err)
	}
	if resp.AccessToken != "jwt_browser_ok" || resp.Organization == nil || resp.Organization.ID != "org_echo" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}
