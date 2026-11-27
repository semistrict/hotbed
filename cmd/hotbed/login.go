package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/cliauth"
)

type loginCmd struct {
	Controller string `arg:"" optional:"" env:"HOTBED_CONTROLLER" help:"Controller HTTPS URL (e.g. https://hotbed.semistrict.com). Defaults to HOTBED_CONTROLLER or your configured controller."`
	Device     bool   `name:"device" help:"Use OAuth 2.0 Device Authorization Grant (for SSH/headless machines without a local browser)."`
	NoOpen     bool   `name:"no-open" help:"Print the login URL without opening a browser."`
	Org        string `name:"org" help:"Optional WorkOS organization ID to sign directly into."`
	Provider   string `name:"provider" help:"WorkOS provider (GoogleOAuth)."`
}

type loginResult struct {
	Controller    string                        `json:"controller"`
	User          cliauth.UserInfo              `json:"user"`
	Organization  *cliauth.OrganizationSummary  `json:"organization,omitempty"`
	Organizations []cliauth.OrganizationSummary `json:"organizations,omitempty"`
}

func (c *loginCmd) Exec(ctx context.Context, d deps) (any, error) {
	ctrl := strings.TrimSpace(c.Controller)
	if ctrl == "" && d.controller != nil {
		addr, err := d.controller()
		if err == nil {
			ctrl = addr
		}
	}
	if ctrl == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "controller URL is required").
			WithSuggestion("run `hotbed login https://hotbed.semistrict.com` or set HOTBED_CONTROLLER")
	}
	base, err := api.ControllerBaseURL(ctrl)
	if err != nil {
		return nil, err
	}
	var opener func(context.Context, string) error
	if !c.NoOpen {
		opener = d.openURL
		if opener == nil {
			opener = openDefaultBrowser
		}
	}
	hc := d.http
	var resp cliauth.TokenResponse
	if c.Device {
		resp, err = cliauth.LoginDevice(ctx, base, opener, d.stderr, hc)
	} else {
		resp, err = cliauth.LoginBrowser(ctx, base, strings.TrimSpace(c.Org), strings.TrimSpace(c.Provider), opener, d.stderr, hc)
	}
	if err != nil {
		return nil, err
	}
	dir, err := defaultHotbedDir()
	if err != nil {
		return nil, err
	}
	creds := cliauth.CredentialsFromResponse(resp)
	if err := cliauth.SaveCredentials(dir, base, creds, true); err != nil {
		return nil, err
	}
	return loginResult{
		Controller:    base,
		User:          resp.User,
		Organization:  resp.Organization,
		Organizations: resp.Organizations,
	}, nil
}

type logoutCmd struct {
	Controller string `arg:"" optional:"" env:"HOTBED_CONTROLLER" help:"Controller URL to sign out of (default: current controller)."`
}

type logoutResult struct {
	Controller string `json:"controller"`
	LoggedOut  bool   `json:"loggedOut"`
}

func (c *logoutCmd) Exec(_ context.Context, d deps) (any, error) {
	ctrl := strings.TrimSpace(c.Controller)
	if ctrl == "" && d.controller != nil {
		if addr, err := d.controller(); err == nil {
			ctrl = addr
		}
	}
	if ctrl == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "no controller configured to sign out of")
	}
	base, err := api.ControllerBaseURL(ctrl)
	if err != nil {
		return nil, err
	}
	dir, err := defaultHotbedDir()
	if err != nil {
		return nil, err
	}
	if err := cliauth.DeleteCredentials(dir, base); err != nil {
		return nil, err
	}
	return logoutResult{Controller: base, LoggedOut: true}, nil
}

type whoamiResponse struct {
	Authenticated bool                          `json:"authenticated"`
	AuthMethod    string                        `json:"authMethod"`
	WorkOSEnabled bool                          `json:"workosEnabled"`
	User          cliauth.UserInfo              `json:"user"`
	Organization  *cliauth.OrganizationSummary  `json:"organization"`
	Organizations []cliauth.OrganizationSummary `json:"organizations"`
	Controller    string                        `json:"controller,omitempty"`
}

type whoamiCmd struct {
	controllerClient
}

func (c *whoamiCmd) Exec(ctx context.Context, d deps) (any, error) {
	var out whoamiResponse
	if err := c.controllerJSON(ctx, d, http.MethodGet, "/auth/me", nil, &out); err != nil {
		return nil, err
	}
	out.Controller = cliauth.NormalizeController(c.Controller)
	return out, nil
}

type orgCmd struct {
	Ls        orgLsCmd        `cmd:"" help:"List organizations you belong to."`
	Switch    orgSwitchCmd    `cmd:"" help:"Switch your active organization (each organization has its own isolated Controller DO)."`
	Create    orgCreateCmd    `cmd:"" help:"Create a new WorkOS organization and switch into it."`
	Members   orgMembersCmd   `cmd:"" help:"List members and pending invitations in the active organization."`
	Invite    orgInviteCmd    `cmd:"" help:"Invite a user by email to the active organization."`
	JoinToken orgJoinTokenCmd `cmd:"" name:"join-token" help:"Mint, list, or revoke org-scoped worker join tokens (hbj_...)."`
	APIToken  orgAPITokenCmd  `cmd:"" name:"api-token" help:"Mint, list, or revoke org-scoped personal API tokens (hbu_...)."`
}

type orgLsCmd struct {
	controllerClient
}

type orgLsResult struct {
	Active        *cliauth.OrganizationSummary  `json:"active,omitempty"`
	Organizations []cliauth.OrganizationSummary `json:"organizations"`
}

func (c *orgLsCmd) Exec(ctx context.Context, d deps) (any, error) {
	var me whoamiResponse
	if err := c.controllerJSON(ctx, d, http.MethodGet, "/auth/me", nil, &me); err != nil {
		return nil, err
	}
	return orgLsResult{Active: me.Organization, Organizations: me.Organizations}, nil
}

type orgSwitchCmd struct {
	controllerClient
	Org string `arg:"" help:"Organization ID (org_...) or organization name to switch to."`
}

type orgSwitchResult struct {
	Organization cliauth.OrganizationSummary `json:"organization"`
}

func (c *orgSwitchCmd) Exec(ctx context.Context, d deps) (any, error) {
	dir, err := defaultHotbedDir()
	if err != nil {
		return nil, err
	}
	base, err := api.ControllerBaseURL(c.Controller)
	if err != nil {
		return nil, err
	}
	key := cliauth.NormalizeController(base)

	target := strings.TrimSpace(c.Org)
	if target == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "organization ID or name is required")
	}
	// Resolve by name first (this may auto-refresh and rotate auth.json under lock).
	if !strings.HasPrefix(target, "org_") {
		var me whoamiResponse
		if err := c.controllerJSON(ctx, d, http.MethodGet, "/auth/me", nil, &me); err == nil {
			for _, o := range me.Organizations {
				if strings.EqualFold(o.Name, target) || o.ID == target {
					target = o.ID
					break
				}
			}
		}
	}

	// Acquire auth.lock and load the current (post-/auth/me) refresh token so we
	// never reuse a rotated token or race with concurrent CLI refreshes.
	resp, err := cliauth.SwitchOrg(ctx, dir, key, target, d.http)
	if err != nil {
		return nil, err
	}
	if resp.Organization == nil {
		return nil, api.Errorf(api.CodeInternal, "controller did not return an active organization")
	}
	return orgSwitchResult{Organization: *resp.Organization}, nil
}

type orgCreateCmd struct {
	controllerClient
	Name string `arg:"" help:"Name of the new organization."`
}

func (c *orgCreateCmd) Exec(ctx context.Context, d deps) (any, error) {
	dir, err := defaultHotbedDir()
	if err != nil {
		return nil, err
	}
	base, err := api.ControllerBaseURL(c.Controller)
	if err != nil {
		return nil, err
	}
	key := cliauth.NormalizeController(base)
	name := strings.TrimSpace(c.Name)

	var out struct {
		Organization cliauth.OrganizationSummary `json:"organization"`
		AccessToken  string                      `json:"accessToken,omitempty"`
		RefreshToken string                      `json:"refreshToken,omitempty"`
		ExpiresAt    int64                       `json:"expiresAt,omitempty"`
	}

	// Coordinate token refresh, /auth/org/create, and saving the newly rotated
	// tokens under auth.lock so we never submit a stale pre-refresh token.
	usedLockedCreds := false
	if err := cliauth.WithFreshCredentials(ctx, dir, key, d.http, func(creds *cliauth.Credentials) (bool, error) {
		if creds.AccessToken == "" {
			return false, nil
		}
		usedLockedCreds = true
		payload := map[string]string{"name": name}
		if creds.RefreshToken != "" {
			payload["refreshToken"] = creds.RefreshToken
		}
		if err := c.controllerJSONWithToken(ctx, d, http.MethodPost, "/auth/org/create", creds.AccessToken, payload, &out); err != nil {
			return false, err
		}
		if out.AccessToken == "" {
			return false, nil
		}
		creds.AccessToken = out.AccessToken
		if out.RefreshToken != "" {
			creds.RefreshToken = out.RefreshToken
		}
		if out.ExpiresAt > 0 {
			creds.ExpiresAt = out.ExpiresAt
		}
		creds.OrganizationID = out.Organization.ID
		creds.OrganizationName = out.Organization.Name
		creds.Role = out.Organization.Role
		return true, nil
	}); err != nil {
		return nil, err
	}

	if !usedLockedCreds {
		payload := map[string]string{"name": name}
		if err := c.controllerJSON(ctx, d, http.MethodPost, "/auth/org/create", payload, &out); err != nil {
			return nil, err
		}
	}
	return orgSwitchResult{Organization: out.Organization}, nil
}

type orgMembersCmd struct {
	controllerClient
}

func (c *orgMembersCmd) Exec(ctx context.Context, d deps) (any, error) {
	var out map[string]any
	if err := c.controllerJSON(ctx, d, http.MethodGet, "/auth/org/members", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

type orgInviteCmd struct {
	controllerClient
	Email string `arg:"" help:"Email address to invite to the active organization."`
	Role  string `name:"role" default:"member" help:"Role slug to assign (e.g. viewer, member, admin)."`
}

func (c *orgInviteCmd) Exec(ctx context.Context, d deps) (any, error) {
	var out map[string]any
	if err := c.controllerJSON(ctx, d, http.MethodPost, "/auth/org/invite", map[string]string{
		"email": strings.TrimSpace(c.Email),
		"role":  strings.TrimSpace(c.Role),
	}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

type orgJoinTokenCmd struct {
	Create orgJoinTokenCreateCmd `cmd:"" help:"Mint a new org-scoped worker join token (hbj_...)."`
	Ls     orgJoinTokenLsCmd     `cmd:"" help:"List worker join tokens for the active organization."`
	Rm     orgJoinTokenRmCmd     `cmd:"" help:"Revoke a worker join token by ID."`
}

type orgJoinTokenCreateCmd struct {
	controllerClient
	Label          string  `name:"label" help:"Optional label for the join token (e.g. linux-fleet)."`
	ExpiresInHours float64 `name:"expires-in-hours" default:"24" help:"Hours until expiry (0 for no expiry)."`
}

func (c *orgJoinTokenCreateCmd) Exec(ctx context.Context, d deps) (any, error) {
	body := map[string]any{"label": strings.TrimSpace(c.Label)}
	if c.ExpiresInHours > 0 {
		body["expiresInHours"] = c.ExpiresInHours
	}
	var out map[string]any
	if err := c.controllerJSON(ctx, d, http.MethodPost, "/admin/join-tokens", body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

type orgJoinTokenLsCmd struct {
	controllerClient
}

func (c *orgJoinTokenLsCmd) Exec(ctx context.Context, d deps) (any, error) {
	var out map[string]any
	if err := c.controllerJSON(ctx, d, http.MethodGet, "/admin/join-tokens", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

type orgJoinTokenRmCmd struct {
	controllerClient
	ID string `arg:"" help:"Join token ID (jt_...) to revoke."`
}

func (c *orgJoinTokenRmCmd) Exec(ctx context.Context, d deps) (any, error) {
	var out map[string]any
	if err := c.controllerJSON(ctx, d, http.MethodDelete, "/admin/join-tokens/"+url.PathEscape(strings.TrimSpace(c.ID)), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

type orgAPITokenCmd struct {
	Create orgAPITokenCreateCmd `cmd:"" help:"Create an org-scoped personal API token (hbu_...) for CI or headless scripts."`
	Ls     orgAPITokenLsCmd     `cmd:"" help:"List personal API tokens."`
	Rm     orgAPITokenRmCmd     `cmd:"" help:"Revoke a personal API token by ID."`
}

type orgAPITokenCreateCmd struct {
	controllerClient
	Name          string  `arg:"" help:"Descriptive name for the API token (e.g. github-ci)."`
	ExpiresInDays float64 `name:"expires-in-days" default:"90" help:"Days until expiry (0 for no expiry)."`
}

func (c *orgAPITokenCreateCmd) Exec(ctx context.Context, d deps) (any, error) {
	body := map[string]any{"name": strings.TrimSpace(c.Name)}
	if c.ExpiresInDays > 0 {
		body["expiresInDays"] = c.ExpiresInDays
	}
	var out map[string]any
	if err := c.controllerJSON(ctx, d, http.MethodPost, "/admin/api-tokens", body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

type orgAPITokenLsCmd struct {
	controllerClient
}

func (c *orgAPITokenLsCmd) Exec(ctx context.Context, d deps) (any, error) {
	var out map[string]any
	if err := c.controllerJSON(ctx, d, http.MethodGet, "/admin/api-tokens", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

type orgAPITokenRmCmd struct {
	controllerClient
	ID string `arg:"" help:"API token ID (at_...) to revoke."`
}

func (c *orgAPITokenRmCmd) Exec(ctx context.Context, d deps) (any, error) {
	var out map[string]any
	if err := c.controllerJSON(ctx, d, http.MethodDelete, "/admin/api-tokens/"+url.PathEscape(strings.TrimSpace(c.ID)), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (f controllerClient) controllerJSON(ctx context.Context, d deps, method, path string, reqBody any, out any) error {
	tok, err := resolveControllerAuthToken(ctx, d.getenv, f.Controller, d.http)
	if err != nil {
		return err
	}
	return f.controllerJSONWithToken(ctx, d, method, path, tok, reqBody, out)
}

func (f controllerClient) controllerJSONWithToken(ctx context.Context, d deps, method, path, tok string, reqBody any, out any) error {
	base, err := api.ControllerBaseURL(f.Controller)
	if err != nil {
		return err
	}
	var bodyReader io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	identify(req.Header, tok)
	hc := d.http
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return api.Errorf(api.CodeControllerUnreachable, "%s %s: %v", method, path, err)
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
				ae.Code = api.CodeInternal
			}
			return &ae
		}
		return api.Errorf(api.CodeInternal, "%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode %s %s: %w", method, path, err)
		}
	}
	return nil
}
