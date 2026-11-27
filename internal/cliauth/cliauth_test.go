package cliauth

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestLoginDeviceFlow(t *testing.T) {
	origFloor := devicePollMinInterval
	devicePollMinInterval = time.Millisecond
	t.Cleanup(func() { devicePollMinInterval = origFloor })

	var polls atomic.Int32
	var seenOrg string
	var openedURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/cli/device/start":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"deviceCode":              "dev_code_123",
				"userCode":                "ABCD-EFGH",
				"verificationUri":         "https://auth.example/device",
				"verificationUriComplete": "https://auth.example/device?user_code=ABCD-EFGH",
				"expiresIn":               300,
				"interval":                1,
			})
		case "/auth/cli/device/poll":
			var body struct {
				DeviceCode     string `json:"deviceCode"`
				OrganizationID string `json:"organizationId"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			if body.DeviceCode != "dev_code_123" {
				http.Error(w, `{"code":"NOT_AUTHORIZED","message":"bad device code"}`, 401)
				return
			}
			seenOrg = body.OrganizationID
			if polls.Add(1) == 1 {
				_ = json.NewEncoder(w).Encode(TokenResponse{Status: "pending"})
				return
			}
			_ = json.NewEncoder(w).Encode(TokenResponse{
				Status:       "complete",
				AccessToken:  "jwt_device_ok",
				RefreshToken: "rt_device_ok",
				ExpiresAt:    time.Now().Add(5 * time.Minute).UnixMilli(),
				User:         UserInfo{ID: "user_1", Email: "ramon@echophase.test", Name: "Ramon"},
				Organization: &OrganizationSummary{ID: "org_echo", Name: "Echophase", Role: "admin"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	var stderr bytes.Buffer
	openURL := func(_ context.Context, target string) error {
		openedURL = target
		return nil
	}

	resp, err := LoginDevice(t.Context(), srv.URL, "org_echo", openURL, &stderr, srv.Client())
	if err != nil {
		t.Fatalf("LoginDevice: %v", err)
	}
	if resp.AccessToken != "jwt_device_ok" || resp.Organization == nil || resp.Organization.ID != "org_echo" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if seenOrg != "org_echo" {
		t.Fatalf("organizationId sent to poll = %q, want org_echo", seenOrg)
	}
	if openedURL != "https://auth.example/device?user_code=ABCD-EFGH" {
		t.Fatalf("openedURL = %q, want complete verification URI", openedURL)
	}
	if !strings.Contains(stderr.String(), "ABCD-EFGH") || !strings.Contains(stderr.String(), "https://auth.example/device?user_code=ABCD-EFGH") {
		t.Fatalf("stderr missing code or verification URL: %s", stderr.String())
	}
}

func TestResolveTokenRejectsWrongOrgWithoutRefreshToken(t *testing.T) {
	dir := t.TempDir()
	controller := "https://controller.test"
	if err := SaveCredentials(dir, controller, Credentials{
		AccessToken:    "jwt_for_org_acme",
		RefreshToken:   "",
		ExpiresAt:      time.Now().Add(10 * time.Minute).UnixMilli(),
		OrganizationID: "org_acme",
	}, true); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}

	tok, err := ResolveToken(t.Context(), dir, controller, "org_beta", nil)
	if err == nil {
		t.Fatalf("ResolveToken(targetOrg=org_beta) without refresh token returned err=nil tok=%q; want error", tok)
	}
}

func TestResolveTokenCachesRefreshWhenTargetOrgMatchesStoredOrg(t *testing.T) {
	dir := t.TempDir()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		_ = json.NewEncoder(w).Encode(TokenResponse{
			AccessToken:  "jwt_fresh",
			RefreshToken: "rt_" + string(rune('0'+n+1)),
			ExpiresAt:    time.Now().Add(10 * time.Minute).UnixMilli(),
			User:         UserInfo{ID: "user_1", Email: "alice@acme.test"},
			Organization: &OrganizationSummary{ID: "org_acme", Name: "Acme", Role: "admin"},
		})
	}))
	defer srv.Close()

	if err := SaveCredentials(dir, srv.URL, Credentials{
		AccessToken:    "jwt_expiring",
		RefreshToken:   "rt_1",
		ExpiresAt:      time.Now().Add(10 * time.Second).UnixMilli(),
		OrganizationID: "org_acme",
	}, true); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}

	if _, err := ResolveToken(t.Context(), dir, srv.URL, "org_acme", srv.Client()); err != nil {
		t.Fatalf("ResolveToken(1): %v", err)
	}
	if _, err := ResolveToken(t.Context(), dir, srv.URL, "org_acme", srv.Client()); err != nil {
		t.Fatalf("ResolveToken(2): %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("ResolveToken with matching targetOrg refreshed %d times; want 1", got)
	}
}
