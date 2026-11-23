// Package ghauth implements GitHub OAuth 2.0 Device Flow (RFC 8628) and
// refresh-token rotation for public desktop/CLI clients, adapted from the
// MIT-licensed GitHub CLI (github.com/cli/cli internal/authflow).
package ghauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// DefaultClientID is Hotbed's public GitHub OAuth App client ID
	// (https://github.com/settings/applications/4044850, Device Flow +
	// expiring user access tokens enabled, no client secret).
	DefaultClientID = "Ov23li2OfqK7WCZDH6qU"

	deviceCodeGrantType = "urn:ietf:params:oauth:grant-type:device_code"
)

// DefaultScopes are the baseline OAuth scopes requested during `hotbed auth login`,
// matching `gh auth login`'s minimum scopes (cli/cli/internal/authflow/flow.go).
var DefaultScopes = []string{"repo", "read:org", "gist"}

// ErrRefreshTokenInvalid indicates GitHub rejected the refresh token and the
// user must run `hotbed auth login` again.
var ErrRefreshTokenInvalid = errors.New("OAuth refresh token rejected by GitHub")

// DeviceCode is GitHub's response to POST /login/device/code.
type DeviceCode struct {
	DeviceCode      string
	UserCode        string
	VerificationURI string
	ExpiresIn       int
	Interval        int
}

// Token is an OAuth user access token (and optional refresh token) returned by
// GitHub's token endpoint.
type Token struct {
	AccessToken      string
	RefreshToken     string
	Scope            string
	TokenType        string
	ExpiresAt        time.Time // zero when non-expiring
	RefreshExpiresAt time.Time // zero when non-expiring or no refresh token
}

// RequestDeviceCode initiates the OAuth Device Flow on oauthBaseURL
// (e.g. "https://github.com").
func RequestDeviceCode(ctx context.Context, hc *http.Client, oauthBaseURL, clientID string, scopes []string) (DeviceCode, error) {
	if clientID == "" {
		clientID = DefaultClientID
	}
	allScopes := mergeScopes(DefaultScopes, scopes)
	form := url.Values{
		"client_id": {clientID},
		"scope":     {strings.Join(allScopes, " ")},
	}
	endpoint := strings.TrimRight(oauthBaseURL, "/") + "/login/device/code"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return DeviceCode{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := hc.Do(req)
	if err != nil {
		return DeviceCode{}, fmt.Errorf("request device code from %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return DeviceCode{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return DeviceCode{}, fmt.Errorf("POST %s: %s: %s", endpoint, resp.Status, strings.TrimSpace(string(body)))
	}
	var raw struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
		Error           string `json:"error"`
		ErrorDesc       string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return DeviceCode{}, fmt.Errorf("decode device code response: %w", err)
	}
	if raw.Error != "" {
		return DeviceCode{}, fmt.Errorf("%s: %s", raw.Error, raw.ErrorDesc)
	}
	if raw.DeviceCode == "" || raw.UserCode == "" || raw.VerificationURI == "" {
		return DeviceCode{}, fmt.Errorf("incomplete device code response from %s", endpoint)
	}
	if raw.Interval <= 0 {
		raw.Interval = 5
	}
	if raw.ExpiresIn <= 0 {
		raw.ExpiresIn = 900
	}
	return DeviceCode{
		DeviceCode:      raw.DeviceCode,
		UserCode:        raw.UserCode,
		VerificationURI: raw.VerificationURI,
		ExpiresIn:       raw.ExpiresIn,
		Interval:        raw.Interval,
	}, nil
}

// PollAccessToken polls GitHub's OAuth token endpoint until the user approves
// the device code, denies it, or the code expires. No client_secret is sent.
func PollAccessToken(ctx context.Context, hc *http.Client, oauthBaseURL, clientID, deviceCode string, interval time.Duration, expiresAt time.Time, now func() time.Time) (Token, error) {
	if now == nil {
		now = time.Now
	}
	if clientID == "" {
		clientID = DefaultClientID
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	endpoint := strings.TrimRight(oauthBaseURL, "/") + "/login/oauth/access_token"
	for {
		if !expiresAt.IsZero() && !now().Before(expiresAt) {
			return Token{}, errors.New("GitHub login timed out: device code expired; run `hotbed auth login` again")
		}
		select {
		case <-ctx.Done():
			return Token{}, ctx.Err()
		case <-time.After(interval):
		}

		// Anchor token lifetime to just before the exchange, matching cli/cli.
		requestedAt := now().UTC()
		form := url.Values{
			"client_id":   {clientID},
			"device_code": {deviceCode},
			"grant_type":  {deviceCodeGrantType},
		}
		tok, pollErr, newInterval, err := exchangeToken(ctx, hc, endpoint, form, requestedAt)
		if err != nil {
			return Token{}, err
		}
		switch pollErr {
		case "":
			return tok, nil
		case "authorization_pending":
			continue
		case "slow_down":
			if newInterval > 0 {
				interval = newInterval
			} else {
				interval += 5 * time.Second
			}
			continue
		case "expired_token":
			return Token{}, errors.New("GitHub login timed out: device code expired; run `hotbed auth login` again")
		case "access_denied":
			return Token{}, errors.New("GitHub login was cancelled by the user")
		default:
			return Token{}, fmt.Errorf("GitHub OAuth login failed: %s", pollErr)
		}
	}
}

// RefreshAccessToken exchanges a device-flow refresh token for a fresh access
// token (and optional rotated refresh token) without a client_secret.
func RefreshAccessToken(ctx context.Context, hc *http.Client, oauthBaseURL, clientID, refreshToken string, now func() time.Time) (Token, error) {
	if now == nil {
		now = time.Now
	}
	if clientID == "" {
		clientID = DefaultClientID
	}
	endpoint := strings.TrimRight(oauthBaseURL, "/") + "/login/oauth/access_token"
	requestedAt := now().UTC()
	form := url.Values{
		"client_id":     {clientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	}
	tok, oauthErr, _, err := exchangeToken(ctx, hc, endpoint, form, requestedAt)
	if err != nil {
		return Token{}, err
	}
	if oauthErr != "" {
		return Token{}, fmt.Errorf("%w: %s", ErrRefreshTokenInvalid, oauthErr)
	}
	// RFC 6749: keep the previous refresh token if the server omits a new one.
	if tok.RefreshToken == "" {
		tok.RefreshToken = refreshToken
	}
	return tok, nil
}

func exchangeToken(ctx context.Context, hc *http.Client, endpoint string, form url.Values, requestedAt time.Time) (Token, string, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, "", 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := hc.Do(req)
	if err != nil {
		return Token{}, "", 0, fmt.Errorf("POST %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return Token{}, "", 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return Token{}, "", 0, fmt.Errorf("POST %s: %s: %s", endpoint, resp.Status, strings.TrimSpace(string(body)))
	}
	var raw struct {
		AccessToken           string `json:"access_token"`
		RefreshToken          string `json:"refresh_token"`
		TokenType             string `json:"token_type"`
		Scope                 string `json:"scope"`
		ExpiresIn             int64  `json:"expires_in"`
		RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"`
		Error                 string `json:"error"`
		ErrorDesc             string `json:"error_description"`
		Interval              int    `json:"interval"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return Token{}, "", 0, fmt.Errorf("decode token response: %w", err)
	}
	if raw.Error != "" {
		var next time.Duration
		if raw.Interval > 0 {
			next = time.Duration(raw.Interval) * time.Second
		}
		if raw.Error == "authorization_pending" || raw.Error == "slow_down" || raw.Error == "expired_token" || raw.Error == "access_denied" {
			return Token{}, raw.Error, next, nil
		}
		if raw.ErrorDesc != "" {
			return Token{}, raw.Error + ": " + raw.ErrorDesc, next, nil
		}
		return Token{}, raw.Error, next, nil
	}
	if raw.AccessToken == "" {
		return Token{}, "", 0, fmt.Errorf("OAuth token response from %s had empty access_token", endpoint)
	}
	tok := Token{
		AccessToken:  raw.AccessToken,
		RefreshToken: raw.RefreshToken,
		Scope:        raw.Scope,
		TokenType:    raw.TokenType,
	}
	if raw.ExpiresIn > 0 {
		tok.ExpiresAt = requestedAt.Add(time.Duration(raw.ExpiresIn) * time.Second)
	}
	if raw.RefreshTokenExpiresIn > 0 {
		tok.RefreshExpiresAt = requestedAt.Add(time.Duration(raw.RefreshTokenExpiresIn) * time.Second)
	}
	return tok, "", 0, nil
}

// CurrentLogin fetches the authenticated GitHub user's login name via GET /user.
func CurrentLogin(ctx context.Context, hc *http.Client, apiBaseURL, accessToken string) (string, error) {
	endpoint := strings.TrimRight(apiBaseURL, "/") + "/user"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("GET %s: %s: %s", endpoint, resp.Status, strings.TrimSpace(string(b)))
	}
	var u struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return "", fmt.Errorf("decode /user response: %w", err)
	}
	return u.Login, nil
}

func mergeScopes(base, extra []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range [][]string{base, extra} {
		for _, s := range list {
			for _, part := range strings.Fields(strings.ReplaceAll(s, ",", " ")) {
				if part != "" && !seen[part] {
					seen[part] = true
					out = append(out, part)
				}
			}
		}
	}
	return out
}
