// Package cliauth manages WorkOS user credentials for the hotbed CLI:
// storing per-controller tokens in ~/.hotbed/auth.json (0600), running the
// OAuth 2.0 Device Authorization flow against the controller, and
// transparently refreshing expiring WorkOS JWTs.
package cliauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/semistrict/hotbed/internal/api"
)

const (
	authFileName  = "auth.json"
	refreshMargin = 60 * time.Second
)

var devicePollMinInterval = time.Second

// Store is the on-disk ~/.hotbed/auth.json document.
type Store struct {
	DefaultController string                 `json:"defaultController,omitempty"`
	Controllers       map[string]Credentials `json:"controllers,omitempty"`
}

// Credentials are a signed-in user's WorkOS tokens and active organization for one controller.
type Credentials struct {
	AccessToken      string `json:"accessToken"`
	RefreshToken     string `json:"refreshToken,omitempty"`
	ExpiresAt        int64  `json:"expiresAt,omitempty"` // unix ms
	UserID           string `json:"userId,omitempty"`
	Email            string `json:"email,omitempty"`
	Name             string `json:"name,omitempty"`
	OrganizationID   string `json:"organizationId,omitempty"`
	OrganizationName string `json:"organizationName,omitempty"`
	Role             string `json:"role,omitempty"`
}

// OrganizationSummary describes a WorkOS organization membership.
type OrganizationSummary struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Role         string `json:"role"`
	MembershipID string `json:"membershipId,omitempty"`
}

// UserInfo describes a WorkOS user returned by the controller.
type UserInfo struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	FirstName string `json:"firstName,omitempty"`
	LastName  string `json:"lastName,omitempty"`
	Name      string `json:"name,omitempty"`
}

// TokenResponse is returned by /auth/cli/exchange, /auth/cli/refresh, and /auth/cli/device/poll.
type TokenResponse struct {
	Status        string                `json:"status,omitempty"`
	AccessToken   string                `json:"accessToken"`
	RefreshToken  string                `json:"refreshToken"`
	ExpiresAt     int64                 `json:"expiresAt"`
	User          UserInfo              `json:"user"`
	Organization  *OrganizationSummary  `json:"organization"`
	Organizations []OrganizationSummary `json:"organizations"`
}

// NormalizeController normalizes a controller base URL key for auth.json.
func NormalizeController(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	base, err := api.ControllerBaseURL(trimmed)
	if err != nil {
		return strings.TrimRight(trimmed, "/")
	}
	return strings.TrimRight(base, "/")
}

func authFilePath(dir string) string {
	return filepath.Join(dir, authFileName)
}

// Load reads ~/.hotbed/auth.json (returning an empty Store when missing).
func Load(dir string) (Store, error) {
	b, err := os.ReadFile(authFilePath(dir))
	if errors.Is(err, fs.ErrNotExist) {
		return Store{Controllers: map[string]Credentials{}}, nil
	}
	if err != nil {
		return Store{}, err
	}
	var s Store
	if err := json.Unmarshal(b, &s); err != nil {
		return Store{}, fmt.Errorf("parse %s: %w", authFilePath(dir), err)
	}
	if s.Controllers == nil {
		s.Controllers = map[string]Credentials{}
	}
	return s, nil
}

// Save atomically writes s to <dir>/auth.json with mode 0600.
func Save(dir string, s Store) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	dst := authFilePath(dir)
	tmp, err := os.CreateTemp(dir, authFileName+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// SaveCredentials updates a single controller entry in auth.json under a file lock.
func SaveCredentials(dir, controller string, creds Credentials, setDefault bool) error {
	key := NormalizeController(controller)
	if key == "" {
		return errors.New("empty controller address")
	}
	return withFileLock(dir, func() error {
		s, err := Load(dir)
		if err != nil {
			return err
		}
		s.Controllers[key] = creds
		if setDefault || s.DefaultController == "" {
			s.DefaultController = key
		}
		return Save(dir, s)
	})
}

// DeleteCredentials removes stored credentials for controller from auth.json.
func DeleteCredentials(dir, controller string) error {
	key := NormalizeController(controller)
	return withFileLock(dir, func() error {
		s, err := Load(dir)
		if err != nil {
			return err
		}
		delete(s.Controllers, key)
		if s.DefaultController == key {
			s.DefaultController = ""
			for k := range s.Controllers {
				s.DefaultController = k
				break
			}
		}
		return Save(dir, s)
	})
}

// DefaultController returns the default controller recorded by `hotbed login`, if any.
func DefaultController(dir string) string {
	s, err := Load(dir)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(s.DefaultController)
}

// CredentialsFromResponse converts a TokenResponse into persisted Credentials.
func CredentialsFromResponse(r TokenResponse) Credentials {
	c := Credentials{
		AccessToken:  r.AccessToken,
		RefreshToken: r.RefreshToken,
		ExpiresAt:    r.ExpiresAt,
		UserID:       r.User.ID,
		Email:        r.User.Email,
		Name:         r.User.Name,
	}
	if r.Organization != nil {
		c.OrganizationID = r.Organization.ID
		c.OrganizationName = r.Organization.Name
		c.Role = r.Organization.Role
	}
	return c
}

// ResolveToken returns a valid bearer access token for controller from
// <dir>/auth.json, transparently refreshing via /auth/cli/refresh when the
// stored WorkOS JWT is within 60 seconds of expiry or when targetOrg differs.
func ResolveToken(ctx context.Context, dir, controller, targetOrg string, hc *http.Client) (string, error) {
	key := NormalizeController(controller)
	if key == "" {
		return "", nil
	}
	s, err := Load(dir)
	if err != nil {
		return "", err
	}
	creds, ok := s.Controllers[key]
	if !ok || creds.AccessToken == "" {
		return "", nil
	}
	if !needsRefresh(creds, targetOrg, time.Now()) {
		return creds.AccessToken, nil
	}
	if creds.RefreshToken == "" {
		if targetOrg != "" && targetOrg != creds.OrganizationID {
			return "", api.Errorf(api.CodeNotAuthorized, "cannot switch to organization %s without a refresh token", targetOrg).
				WithSuggestion("run `hotbed login` to sign in again")
		}
		return creds.AccessToken, nil
	}
	var refreshedToken string
	err = withFileLock(dir, func() error {
		// Re-read after acquiring lock in case a sibling CLI process already refreshed.
		s2, err := Load(dir)
		if err != nil {
			return err
		}
		c2, ok := s2.Controllers[key]
		if !ok || c2.AccessToken == "" {
			return nil
		}
		if !needsRefresh(c2, targetOrg, time.Now()) {
			refreshedToken = c2.AccessToken
			return nil
		}
		if c2.RefreshToken == "" {
			if targetOrg != "" && targetOrg != c2.OrganizationID {
				return api.Errorf(api.CodeNotAuthorized, "cannot switch to organization %s without a refresh token", targetOrg).
					WithSuggestion("run `hotbed login` to sign in again")
			}
			refreshedToken = c2.AccessToken
			return nil
		}
		orgID := targetOrg
		if orgID == "" {
			orgID = c2.OrganizationID
		}
		resp, err := Refresh(ctx, key, c2.RefreshToken, orgID, hc)
		if err != nil {
			return err
		}
		next := CredentialsFromResponse(resp)
		// Preserve existing stored fields when only switching/refreshing.
		if next.RefreshToken == "" {
			next.RefreshToken = c2.RefreshToken
		}
		if targetOrg == "" || targetOrg == c2.OrganizationID {
			s2.Controllers[key] = next
			if err := Save(dir, s2); err != nil {
				return err
			}
		} else {
			// When HOTBED_ORG overrides org for one command, still persist the rotated
			// refresh token so the old one is not reused and rejected by WorkOS.
			c2.RefreshToken = next.RefreshToken
			s2.Controllers[key] = c2
			if err := Save(dir, s2); err != nil {
				return err
			}
		}
		refreshedToken = next.AccessToken
		return nil
	})
	if err != nil {
		return "", err
	}
	return refreshedToken, nil
}

// SwitchOrg atomically refreshes into targetOrg under the auth.json file lock
// and persists the resulting credentials, preventing concurrent refresh-token
// reuse races with sibling CLI commands.
func SwitchOrg(ctx context.Context, dir, controller, targetOrg string, hc *http.Client) (TokenResponse, error) {
	key := NormalizeController(controller)
	if key == "" {
		return TokenResponse{}, errors.New("empty controller address")
	}
	var resp TokenResponse
	err := withFileLock(dir, func() error {
		s, err := Load(dir)
		if err != nil {
			return err
		}
		creds, ok := s.Controllers[key]
		if !ok || creds.RefreshToken == "" {
			return api.Errorf(api.CodeNotAuthorized, "no WorkOS refresh token stored for %s", key).
				WithSuggestion("run `hotbed login` first")
		}
		r, err := Refresh(ctx, key, creds.RefreshToken, targetOrg, hc)
		if err != nil {
			return err
		}
		next := CredentialsFromResponse(r)
		if next.RefreshToken == "" {
			next.RefreshToken = creds.RefreshToken
		}
		s.Controllers[key] = next
		if err := Save(dir, s); err != nil {
			return err
		}
		resp = r
		return nil
	})
	if err != nil {
		return TokenResponse{}, err
	}
	return resp, nil
}

// WithFreshCredentials acquires the auth.json file lock, refreshes any
// near-expiry stored WorkOS credentials for controller (persisting the rotated
// refresh token immediately), and invokes fn with the fresh credentials. If fn
// returns updated=true, the mutated credentials are saved before releasing the lock.
func WithFreshCredentials(
	ctx context.Context,
	dir, controller string,
	hc *http.Client,
	fn func(creds *Credentials) (updated bool, err error),
) error {
	key := NormalizeController(controller)
	if key == "" {
		return errors.New("empty controller address")
	}
	return withFileLock(dir, func() error {
		s, err := Load(dir)
		if err != nil {
			return err
		}
		creds, ok := s.Controllers[key]
		if ok && creds.AccessToken != "" && creds.RefreshToken != "" && needsRefresh(creds, "", time.Now()) {
			resp, err := Refresh(ctx, key, creds.RefreshToken, creds.OrganizationID, hc)
			if err != nil {
				return err
			}
			next := CredentialsFromResponse(resp)
			if next.RefreshToken == "" {
				next.RefreshToken = creds.RefreshToken
			}
			s.Controllers[key] = next
			if err := Save(dir, s); err != nil {
				return err
			}
			creds = next
		}
		updated, err := fn(&creds)
		if err != nil {
			return err
		}
		if updated {
			s.Controllers[key] = creds
			return Save(dir, s)
		}
		return nil
	})
}

func needsRefresh(c Credentials, targetOrg string, now time.Time) bool {
	if targetOrg != "" && targetOrg != c.OrganizationID {
		return true
	}
	if c.ExpiresAt <= 0 {
		return false
	}
	return now.Add(refreshMargin).UnixMilli() >= c.ExpiresAt
}

// Refresh calls POST <controller>/auth/cli/refresh to obtain a new WorkOS token pair
// (and optionally switch active organization).
func Refresh(ctx context.Context, controllerBaseURL, refreshToken, organizationID string, hc *http.Client) (TokenResponse, error) {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	payload := map[string]string{"refreshToken": refreshToken}
	if organizationID != "" {
		payload["organizationId"] = organizationID
	}
	var out TokenResponse
	if err := postJSON(ctx, hc, strings.TrimRight(controllerBaseURL, "/")+"/auth/cli/refresh", "", payload, &out); err != nil {
		return TokenResponse{}, err
	}
	return out, nil
}

// LoginDevice runs the OAuth 2.0 Device Authorization Grant flow via the controller's
// /auth/cli/device/start and /auth/cli/device/poll endpoints (works identically locally
// and over SSH without any localhost callback listener).
func LoginDevice(
	ctx context.Context,
	controllerBaseURL, organizationID string,
	openURL func(context.Context, string) error,
	stderr io.Writer,
	hc *http.Client,
) (TokenResponse, error) {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	base := strings.TrimRight(controllerBaseURL, "/")
	var start struct {
		DeviceCode              string `json:"deviceCode"`
		UserCode                string `json:"userCode"`
		VerificationURI         string `json:"verificationUri"`
		VerificationURIComplete string `json:"verificationUriComplete"`
		ExpiresIn               int    `json:"expiresIn"`
		Interval                int    `json:"interval"`
	}
	if err := postJSON(ctx, hc, base+"/auth/cli/device/start", "", map[string]any{}, &start); err != nil {
		return TokenResponse{}, err
	}
	verifyURL := strings.TrimSpace(start.VerificationURIComplete)
	if verifyURL == "" {
		verifyURL = strings.TrimSpace(start.VerificationURI)
	}
	if stderr != nil {
		fmt.Fprintf(stderr, "Open this URL in your browser to sign in to hotbed:\n  %s\n", verifyURL)
		if start.UserCode != "" {
			fmt.Fprintf(stderr, "One-time code: %s\n", start.UserCode)
		}
	}
	if openURL != nil && verifyURL != "" {
		if oerr := openURL(ctx, verifyURL); oerr != nil && stderr != nil {
			fmt.Fprintf(stderr, "Could not open browser automatically: %v\n", oerr)
		}
	}
	if stderr != nil {
		fmt.Fprintf(stderr, "Waiting for authorization...\n")
	}

	interval := 5 * time.Second
	if start.Interval > 0 {
		interval = time.Duration(start.Interval) * time.Second
	}
	if interval < devicePollMinInterval {
		interval = devicePollMinInterval
	}
	deadline := time.Now().Add(time.Duration(max(start.ExpiresIn, 300)) * time.Second)
	pollBody := map[string]string{"deviceCode": start.DeviceCode}
	if orgID := strings.TrimSpace(organizationID); orgID != "" {
		pollBody["organizationId"] = orgID
	}
	for {
		if time.Now().After(deadline) {
			return TokenResponse{}, api.Errorf(api.CodeNotAuthorized, "device login timed out before authorization completed")
		}
		select {
		case <-ctx.Done():
			return TokenResponse{}, ctx.Err()
		case <-time.After(interval):
		}
		var poll TokenResponse
		if err := postJSON(ctx, hc, base+"/auth/cli/device/poll", "", pollBody, &poll); err != nil {
			return TokenResponse{}, err
		}
		switch poll.Status {
		case "complete":
			return poll, nil
		case "slow_down":
			interval += 5 * time.Second
		case "pending":
		default:
			if poll.AccessToken != "" {
				return poll, nil
			}
		}
	}
}

func postJSON(ctx context.Context, hc *http.Client, endpoint, bearer string, reqBody any, out any) error {
	b, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return api.Errorf(api.CodeControllerUnreachable, "POST %s: %v", endpoint, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var ae api.Error
		if json.Unmarshal(raw, &ae) == nil && ae.Message != "" {
			if ae.Code == "" {
				ae.Code = api.CodeNotAuthorized
			}
			return &ae
		}
		return api.Errorf(api.CodeNotAuthorized, "POST %s: HTTP %d: %s", endpoint, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}
