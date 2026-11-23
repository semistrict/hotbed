package ghauth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDeviceFlowAndRefreshPreservesRefreshToken(t *testing.T) {
	now := time.Date(2026, 11, 22, 14, 0, 0, 0, time.UTC)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/login/device/code":
			_ = r.ParseForm()
			if !strings.Contains(r.Form.Get("scope"), "repo") || !strings.Contains(r.Form.Get("scope"), "workflow") {
				t.Errorf("unexpected scope %q", r.Form.Get("scope"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code":      "dev-1",
				"user_code":        "1234-ABCD",
				"verification_uri": "https://github.com/login/device",
				"expires_in":       600,
				"interval":         1,
			})
		case "/login/oauth/access_token":
			_ = r.ParseForm()
			switch r.Form.Get("grant_type") {
			case "urn:ietf:params:oauth:grant-type:device_code":
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "access_denied"})
			case "refresh_token":
				// Omit refresh_token to verify caller keeps the existing refresh token.
				_ = json.NewEncoder(w).Encode(map[string]any{
					"access_token": "ghu_new",
					"expires_in":   3600,
				})
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)

	dc, err := RequestDeviceCode(t.Context(), ts.Client(), ts.URL, DefaultClientID, []string{"workflow"})
	if err != nil {
		t.Fatal(err)
	}
	if dc.UserCode != "1234-ABCD" {
		t.Fatalf("UserCode = %q, want 1234-ABCD", dc.UserCode)
	}

	if _, err := PollAccessToken(t.Context(), ts.Client(), ts.URL, DefaultClientID, dc.DeviceCode, time.Millisecond, now.Add(time.Minute), func() time.Time { return now }); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("PollAccessToken access_denied = %v, want cancelled error", err)
	}

	refreshed, err := RefreshAccessToken(t.Context(), ts.Client(), ts.URL, DefaultClientID, "ghr_keep_me", func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.AccessToken != "ghu_new" || refreshed.RefreshToken != "ghr_keep_me" || !refreshed.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("unexpected refreshed token: %+v", refreshed)
	}
}
