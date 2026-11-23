package controller

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
	"github.com/semistrict/hotbed/internal/ghauth"
	"github.com/semistrict/hotbed/internal/httpx"
)

const (
	// gitTokenLeaseTTL is the lease advertised to callers for non-expiring tokens.
	gitTokenLeaseTTL = 15 * time.Minute
	// oauthRefreshLead refreshes an expiring OAuth token this long before expiry.
	oauthRefreshLead = 5 * time.Minute
	// gitHTTPSUsername is the HTTPS username accepted by GitHub for OAuth and PAT tokens.
	gitHTTPSUsername = "x-access-token"
	// maxProxyResponseBytes caps proxied GitHub API responses.
	maxProxyResponseBytes = 16 << 20
)

type pendingGitHubLogin struct {
	name       string
	host       string
	repos      []string
	clientID   string
	deviceCode string
	interval   time.Duration
	expiresAt  time.Time
	apiURL     string
	oauthURL   string
}

type gitAuth struct {
	hotbedv1connect.UnimplementedGitAuthServiceHandler
	s *Server

	mu         sync.Mutex
	logins     map[string]*pendingGitHubLogin
	refreshMu  map[string]*sync.Mutex
}

func newGitAuth(s *Server) *gitAuth {
	return &gitAuth{
		s:         s,
		logins:    map[string]*pendingGitHubLogin{},
		refreshMu: map[string]*sync.Mutex{},
	}
}

func (g *gitAuth) credRefreshLock(name string) *sync.Mutex {
	g.mu.Lock()
	defer g.mu.Unlock()
	m := g.refreshMu[name]
	if m == nil {
		m = &sync.Mutex{}
		g.refreshMu[name] = m
	}
	return m
}

func (g *gitAuth) StartGitHubLogin(ctx context.Context, req *pb.StartGitHubLoginRequest) (*pb.GitHubLoginChallenge, error) {
	name := strings.TrimSpace(req.GetName())
	if name == "" {
		name = "github"
	}
	if !repoID.MatchString(name) {
		return nil, api.Errorf(api.CodeInvalidArgs, "credential name %q must be lowercase letters, digits and dashes", name)
	}
	host, err := validateGitHost(req.GetHost())
	if err != nil {
		return nil, err
	}
	repos, err := normalizeRepoPatterns(req.GetRepos())
	if err != nil {
		return nil, err
	}
	apiURL, err := validateOptionalURL(req.GetApiUrl(), "api_url")
	if err != nil {
		return nil, err
	}
	oauthURL, err := validateOptionalURL(req.GetOauthUrl(), "oauth_url")
	if err != nil {
		return nil, err
	}
	clientID := strings.TrimSpace(req.GetClientId())
	if clientID == "" {
		clientID = ghauth.DefaultClientID
	}

	oauthBase := githubOAuthBase(host, oauthURL)
	dc, err := ghauth.RequestDeviceCode(ctx, g.s.cfg.GitAuthHTTP, oauthBase, clientID, req.GetScopes())
	if err != nil {
		return nil, api.Errorf(api.CodeInternal, "start GitHub OAuth device flow: %v", err)
	}

	b := make([]byte, 12)
	_, _ = rand.Read(b)
	loginID := hex.EncodeToString(b)
	now := g.s.now()

	g.mu.Lock()
	for id, p := range g.logins {
		if now.After(p.expiresAt) {
			delete(g.logins, id)
		}
	}
	g.logins[loginID] = &pendingGitHubLogin{
		name:       name,
		host:       host,
		repos:      repos,
		clientID:   clientID,
		deviceCode: dc.DeviceCode,
		interval:   time.Duration(dc.Interval) * time.Second,
		expiresAt:  now.Add(time.Duration(dc.ExpiresIn) * time.Second),
		apiURL:     apiURL,
		oauthURL:   oauthURL,
	}
	g.mu.Unlock()

	return &pb.GitHubLoginChallenge{
		LoginId:         loginID,
		UserCode:        dc.UserCode,
		VerificationUri: dc.VerificationURI,
		ExpiresIn:       int32(dc.ExpiresIn),
		Interval:        int32(dc.Interval),
	}, nil
}

func (g *gitAuth) CompleteGitHubLogin(ctx context.Context, req *pb.CompleteGitHubLoginRequest) (*pb.GitCredential, error) {
	loginID := strings.TrimSpace(req.GetLoginId())
	if loginID == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "login_id is required").
			WithSuggestion("run `hotbed auth login` or pass --login-id from `hotbed auth login-start`")
	}

	g.mu.Lock()
	pending, ok := g.logins[loginID]
	if ok {
		delete(g.logins, loginID)
	}
	g.mu.Unlock()
	if !ok {
		return nil, api.Errorf(api.CodeNotFound, "no pending GitHub login %q (it may have expired or already completed)", loginID).
			WithSuggestion("run `hotbed auth login` to start a new login")
	}

	oauthBase := githubOAuthBase(pending.host, pending.oauthURL)
	tok, err := ghauth.PollAccessToken(ctx, g.s.cfg.GitAuthHTTP, oauthBase, pending.clientID, pending.deviceCode, pending.interval, pending.expiresAt, g.s.now)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, api.Errorf(api.CodeNotAuthorized, "%v", err)
	}

	apiBase := githubAPIBase(pending.host, pending.apiURL)
	login, err := ghauth.CurrentLogin(ctx, g.s.cfg.GitAuthHTTP, apiBase, tok.AccessToken)
	if err != nil {
		return nil, api.Errorf(api.CodeNotAuthorized, "authenticated with GitHub, but failed to read user profile from %s/user: %v", apiBase, err)
	}

	cred := &pb.GitCredential{
		Name:        pending.name,
		Host:        pending.host,
		Repos:       pending.repos,
		Username:    login,
		ApiUrl:      pending.apiURL,
		OauthUrl:    pending.oauthURL,
		Refreshable: tok.RefreshToken != "",
		UpdatedAt:   g.s.now().UnixMilli(),
		UpdatedBy:   httpx.Peer(ctx).Hostname,
	}
	sec := gitCredentialSecret{
		Token:        tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ClientID:     pending.clientID,
	}
	if !tok.ExpiresAt.IsZero() {
		cred.ExpiresAt = tok.ExpiresAt.UnixMilli()
		sec.TokenExpiresAt = tok.ExpiresAt.UnixMilli()
	}
	if !tok.RefreshExpiresAt.IsZero() {
		sec.RefreshExpiresAt = tok.RefreshExpiresAt.UnixMilli()
	}

	if err := g.s.cfg.Store.PutGitCredential(ctx, cred, sec); err != nil {
		return nil, err
	}
	return cred, nil
}

func (g *gitAuth) PutGitCredential(ctx context.Context, req *pb.PutGitCredentialRequest) (*pb.GitCredential, error) {
	name := strings.TrimSpace(req.GetName())
	if !repoID.MatchString(name) {
		return nil, api.Errorf(api.CodeInvalidArgs, "credential name %q must be lowercase letters, digits and dashes", req.GetName())
	}
	host, err := validateGitHost(req.GetHost())
	if err != nil {
		return nil, err
	}
	repos, err := normalizeRepoPatterns(req.GetRepos())
	if err != nil {
		return nil, err
	}
	apiURL, err := validateOptionalURL(req.GetApiUrl(), "api_url")
	if err != nil {
		return nil, err
	}

	token := strings.TrimSpace(req.GetToken())
	if token == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "token is required (--token or --token-file -)").
			WithSuggestion("use `hotbed auth login` for interactive OAuth login, or `gh auth token | hotbed auth set github --token-file -`")
	}
	if strings.ContainsAny(token, "\r\n\x00") {
		return nil, api.Errorf(api.CodeInvalidArgs, "token must be a single line with no control characters")
	}

	cred := &pb.GitCredential{
		Name:      name,
		Host:      host,
		Repos:     repos,
		Username:  strings.TrimSpace(req.GetUsername()),
		ApiUrl:    apiURL,
		UpdatedAt: g.s.now().UnixMilli(),
		UpdatedBy: httpx.Peer(ctx).Hostname,
	}
	sec := gitCredentialSecret{Token: token}
	if err := g.s.cfg.Store.PutGitCredential(ctx, cred, sec); err != nil {
		return nil, err
	}
	return cred, nil
}

func (g *gitAuth) ListGitCredentials(ctx context.Context, req *pb.ListGitCredentialsRequest) (*pb.ListGitCredentialsResponse, error) {
	creds, err := g.s.cfg.Store.GitCredentials(ctx)
	if err != nil {
		return nil, err
	}
	host := strings.ToLower(strings.TrimSpace(req.GetHost()))
	out := &pb.ListGitCredentialsResponse{}
	for _, c := range creds {
		if host != "" && c.Host != host {
			continue
		}
		out.Credentials = append(out.Credentials, c)
	}
	return out, nil
}

func (g *gitAuth) DeleteGitCredential(ctx context.Context, req *pb.DeleteGitCredentialRequest) (*pb.DeleteGitCredentialResponse, error) {
	name := strings.TrimSpace(req.GetName())
	if name == "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "credential name is required")
	}
	found, err := g.s.cfg.Store.DeleteGitCredential(ctx, name)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, api.Errorf(api.CodeNotFound, "no git credential %q", name).WithSuggestion("hotbed auth ls")
	}
	return &pb.DeleteGitCredentialResponse{Name: name}, nil
}

func (g *gitAuth) MintGitToken(ctx context.Context, req *pb.MintGitTokenRequest) (*pb.GitToken, error) {
	host, owner, repo, err := parseGitRepo(req.GetRepo())
	if err != nil {
		return nil, err
	}
	fullRepo := owner + "/" + repo
	best, err := g.resolveCredential(ctx, host, owner, repo)
	if err != nil {
		return nil, err
	}
	tok, exp, err := g.freshToken(ctx, best)
	if err != nil {
		return nil, err
	}
	return &pb.GitToken{
		Host:       host,
		Repository: fullRepo,
		Username:   gitHTTPSUsername,
		Token:      tok,
		ExpiresAt:  exp.UnixMilli(),
		Credential: best.cred.Name,
	}, nil
}

func (g *gitAuth) ProxyGitHubAPI(ctx context.Context, req *pb.ProxyGitHubAPIRequest) (*pb.ProxyGitHubAPIResponse, error) {
	path := strings.TrimSpace(req.GetPath())
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.Contains(path, "://") {
		return nil, api.Errorf(api.CodeInvalidArgs, "GitHub API path %q must start with a single / (e.g. /user, /repos/owner/repo, or /graphql)", req.GetPath())
	}
	host, err := validateGitHost(req.GetHost())
	if err != nil {
		return nil, err
	}
	method := strings.ToUpper(strings.TrimSpace(req.GetMethod()))
	if method == "" {
		if req.GetBody() != "" {
			method = http.MethodPost
		} else {
			method = http.MethodGet
		}
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return nil, api.Errorf(api.CodeInvalidArgs, "unsupported HTTP method %q", method)
	}

	owner, repo := extractRepoHint(req.GetRepo(), path)
	best, err := g.resolveCredential(ctx, host, owner, repo)
	if err != nil {
		return nil, err
	}
	tok, _, err := g.freshToken(ctx, best)
	if err != nil {
		return nil, err
	}

	target := githubAPIBase(best.cred.Host, best.cred.ApiUrl) + path
	var bodyReader io.Reader
	if req.GetBody() != "" {
		bodyReader = strings.NewReader(req.GetBody())
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, target, bodyReader)
	if err != nil {
		return nil, api.Errorf(api.CodeInvalidArgs, "build GitHub API request: %v", err)
	}

	hasAccept := false
	hasContentType := false
	hasAPIVersion := false
	for _, h := range req.GetHeaders() {
		k, v, ok := strings.Cut(h, ":")
		if !ok {
			return nil, api.Errorf(api.CodeInvalidArgs, "header %q must be \"Key: Value\"", h)
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		switch strings.ToLower(k) {
		case "", "authorization", "cookie", "host", "content-length":
			continue
		case "accept":
			hasAccept = true
		case "content-type":
			hasContentType = true
		case "x-github-api-version":
			hasAPIVersion = true
		}
		httpReq.Header.Add(k, v)
	}
	if !hasAccept {
		httpReq.Header.Set("Accept", "application/vnd.github+json")
	}
	if !hasContentType && req.GetBody() != "" {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	if !hasAPIVersion {
		httpReq.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	httpReq.Header.Set("Authorization", "Bearer "+tok)

	resp, err := g.s.cfg.GitAuthHTTP.Do(httpReq)
	if err != nil {
		return nil, api.Errorf(api.CodeInternal, "GitHub API %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxProxyResponseBytes))
	if err != nil {
		return nil, api.Errorf(api.CodeInternal, "read GitHub API response: %v", err)
	}

	var outHeaders []string
	for k, vals := range resp.Header {
		switch strings.ToLower(k) {
		case "set-cookie", "authorization":
			continue
		}
		for _, v := range vals {
			outHeaders = append(outHeaders, k+": "+v)
		}
	}
	slices.Sort(outHeaders)
	return &pb.ProxyGitHubAPIResponse{
		StatusCode: int32(resp.StatusCode),
		Headers:    outHeaders,
		Body:       string(respBody),
	}, nil
}

func (g *gitAuth) resolveCredential(ctx context.Context, host, owner, repo string) (storedGitCredential, error) {
	candidates, err := g.s.cfg.Store.GitCredentialsByHost(ctx, host)
	if err != nil {
		return storedGitCredential{}, err
	}
	if owner != "" && repo != "" {
		if best, ok := selectGitCredential(candidates, owner, repo); ok {
			return best, nil
		}
		return storedGitCredential{}, api.Errorf(api.CodeNotFound, "no git credential configured for %s/%s/%s", host, owner, repo).
			WithSuggestion("run `hotbed auth login` on the controller (or `gh auth token | hotbed auth set github --token-file -`)")
	}
	// No specific repo (e.g. /user or /graphql): prefer host-wide credentials, else first alphabetically.
	for _, c := range candidates {
		if len(c.cred.Repos) == 0 {
			return c, nil
		}
	}
	if len(candidates) > 0 {
		return candidates[0], nil
	}
	return storedGitCredential{}, api.Errorf(api.CodeNotFound, "no git credential configured for %s", host).
		WithSuggestion("run `hotbed auth login` on the controller (or `gh auth token | hotbed auth set github --token-file -`)")
}

// freshToken returns a valid access token for stored, transparently refreshing
// an expiring OAuth token via its refresh_token when within oauthRefreshLead of expiry.
func (g *gitAuth) freshToken(ctx context.Context, stored storedGitCredential) (string, time.Time, error) {
	now := g.s.now()
	if stored.secret.RefreshToken == "" || stored.secret.TokenExpiresAt == 0 {
		return stored.secret.Token, now.Add(gitTokenLeaseTTL), nil
	}
	exp := time.UnixMilli(stored.secret.TokenExpiresAt)
	if exp.After(now.Add(oauthRefreshLead)) {
		return stored.secret.Token, exp, nil
	}

	mu := g.credRefreshLock(stored.cred.Name)
	mu.Lock()
	defer mu.Unlock()

	// Re-read from DB in case another goroutine already refreshed and rotated it.
	if rows, err := g.s.cfg.Store.GitCredentialsByHost(ctx, stored.cred.Host); err == nil {
		for _, r := range rows {
			if r.cred.Name == stored.cred.Name {
				stored = r
				break
			}
		}
	}
	now = g.s.now()
	if stored.secret.TokenExpiresAt > 0 {
		exp = time.UnixMilli(stored.secret.TokenExpiresAt)
		if exp.After(now.Add(oauthRefreshLead)) {
			return stored.secret.Token, exp, nil
		}
	}

	oauthBase := githubOAuthBase(stored.cred.Host, stored.cred.OauthUrl)
	clientID := cmp.Or(stored.secret.ClientID, ghauth.DefaultClientID)
	refreshed, err := ghauth.RefreshAccessToken(ctx, g.s.cfg.GitAuthHTTP, oauthBase, clientID, stored.secret.RefreshToken, g.s.now)
	if err != nil {
		return "", time.Time{}, api.Errorf(api.CodeNotAuthorized, "refresh GitHub OAuth token for %s: %v", stored.cred.Name, err).
			WithSuggestion("re-authenticate on the controller with: hotbed auth login")
	}

	stored.secret.Token = refreshed.AccessToken
	stored.secret.RefreshToken = refreshed.RefreshToken
	if !refreshed.ExpiresAt.IsZero() {
		stored.secret.TokenExpiresAt = refreshed.ExpiresAt.UnixMilli()
		stored.cred.ExpiresAt = refreshed.ExpiresAt.UnixMilli()
		exp = refreshed.ExpiresAt
	} else {
		stored.secret.TokenExpiresAt = 0
		stored.cred.ExpiresAt = 0
		exp = now.Add(gitTokenLeaseTTL)
	}
	if !refreshed.RefreshExpiresAt.IsZero() {
		stored.secret.RefreshExpiresAt = refreshed.RefreshExpiresAt.UnixMilli()
	}
	stored.cred.Refreshable = stored.secret.RefreshToken != ""
	stored.cred.UpdatedAt = now.UnixMilli()

	if err := g.s.cfg.Store.PutGitCredential(ctx, stored.cred, stored.secret); err != nil {
		return "", time.Time{}, fmt.Errorf("persist refreshed GitHub token: %w", err)
	}
	return stored.secret.Token, exp, nil
}

func githubAPIBase(host, override string) string {
	if override != "" {
		return strings.TrimRight(override, "/")
	}
	if host == "github.com" {
		return "https://api.github.com"
	}
	return "https://" + host + "/api/v3"
}

func githubOAuthBase(host, override string) string {
	if override != "" {
		return strings.TrimRight(override, "/")
	}
	return "https://" + host
}

func validateGitHost(raw string) (string, error) {
	host := strings.ToLower(strings.TrimSpace(raw))
	if host == "" {
		return "github.com", nil
	}
	if strings.ContainsAny(host, " \t\r\n/") || strings.Contains(host, "://") {
		return "", api.Errorf(api.CodeInvalidArgs, "git host %q must be a hostname (or host:port), not a URL", raw)
	}
	return host, nil
}

func validateOptionalURL(raw, field string) (string, error) {
	s := strings.TrimRight(strings.TrimSpace(raw), "/")
	if s == "" {
		return "", nil
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", api.Errorf(api.CodeInvalidArgs, "%s %q must be an http:// or https:// URL", field, raw)
	}
	return s, nil
}

func extractRepoHint(repoHint, apiPath string) (string, string) {
	if s := strings.Trim(strings.TrimSpace(repoHint), "/"); s != "" {
		s = strings.TrimSuffix(s, ".git")
		if o, r, ok := strings.Cut(s, "/"); ok && o != "" && r != "" {
			return o, r
		}
	}
	p, _, _ := strings.Cut(apiPath, "?")
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	if len(parts) >= 3 && parts[0] == "repos" && parts[1] != "" && parts[2] != "" {
		return parts[1], parts[2]
	}
	return "", ""
}

func normalizeRepoPatterns(patterns []string) ([]string, error) {
	if len(patterns) == 0 {
		return nil, nil
	}
	var out []string
	for _, raw := range patterns {
		p := strings.TrimSpace(raw)
		p = strings.Trim(p, "/")
		p = strings.TrimSuffix(p, ".git")
		owner, repo, ok := strings.Cut(p, "/")
		if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") || strings.ContainsAny(p, " \t\r\n") || owner == "*" || owner == "." || owner == ".." || repo == "." || repo == ".." {
			return nil, api.Errorf(api.CodeInvalidArgs, "repo pattern %q must be \"owner/repo\" or \"owner/*\"", raw)
		}
		if !slices.ContainsFunc(out, func(existing string) bool { return strings.EqualFold(existing, p) }) {
			out = append(out, p)
		}
	}
	return out, nil
}

// selectGitCredential chooses the most specific matching credential for owner/repo:
// exact owner/repo (3) > owner/* wildcard (2) > host-wide (1), breaking ties by name.
func selectGitCredential(candidates []storedGitCredential, owner, repo string) (storedGitCredential, bool) {
	var best storedGitCredential
	bestScore := 0
	for _, c := range candidates {
		score := matchScore(c.cred.Repos, owner, repo)
		if score == 0 {
			continue
		}
		if score > bestScore || (score == bestScore && best.cred != nil && c.cred.Name < best.cred.Name) {
			best, bestScore = c, score
		}
	}
	return best, bestScore > 0
}

func matchScore(patterns []string, owner, repo string) int {
	if len(patterns) == 0 {
		return 1
	}
	full := owner + "/" + repo
	wild := owner + "/*"
	best := 0
	for _, p := range patterns {
		switch {
		case strings.EqualFold(p, full):
			return 3
		case strings.EqualFold(p, wild) && best < 2:
			best = 2
		}
	}
	return best
}

// parseGitRepo extracts (host, owner, repo) from an HTTPS/HTTP/SSH URL, SCP remote,
// or canonical "host/owner/repo" string. Standard ports (443/80/22) are stripped;
// custom ports (e.g. 127.0.0.1:54321) are kept.
func parseGitRepo(raw string) (host, owner, repo string, err error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", "", "", api.Errorf(api.CodeInvalidArgs, "repository URL or remote is required")
	}
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, ".") || strings.HasPrefix(s, "file://") {
		return "", "", "", api.Errorf(api.CodeInvalidArgs, "%q is a local path, not a remote git repository", raw)
	}

	var pathPart string
	if strings.Contains(s, "://") {
		u, perr := url.Parse(s)
		if perr != nil || u.Host == "" {
			return "", "", "", api.Errorf(api.CodeInvalidArgs, "unreadable git repository URL %q", raw)
		}
		host = normalizeHost(u.Scheme, u.Host)
		pathPart = u.Path
	} else if h, p, ok := splitSCP(s); ok {
		host = strings.ToLower(h)
		pathPart = p
	} else {
		h, p, ok := strings.Cut(s, "/")
		if !ok || h == "" || p == "" {
			return "", "", "", api.Errorf(api.CodeInvalidArgs, "repository %q must be a URL or host/owner/repo", raw)
		}
		host = strings.ToLower(h)
		pathPart = p
	}

	pathPart = strings.Trim(pathPart, "/")
	pathPart = strings.TrimSuffix(pathPart, ".git")
	parts := strings.Split(pathPart, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", "", api.Errorf(api.CodeInvalidArgs, "repository %q must include owner/repo", raw)
	}
	for _, seg := range parts {
		if seg == "" || seg == "." || seg == ".." {
			return "", "", "", api.Errorf(api.CodeInvalidArgs, "repository %q has an invalid path", raw)
		}
	}
	return host, parts[0], strings.Join(parts[1:], "/"), nil
}

func normalizeHost(scheme, hostport string) string {
	h := strings.ToLower(hostport)
	switch strings.ToLower(scheme) {
	case "https":
		h = strings.TrimSuffix(h, ":443")
	case "http":
		h = strings.TrimSuffix(h, ":80")
	case "ssh":
		h = strings.TrimSuffix(h, ":22")
	}
	return h
}

// splitSCP parses [user@]host:path when s is SCP-style rather than host:port/path.
func splitSCP(s string) (host, path string, ok bool) {
	colon := strings.IndexByte(s, ':')
	if colon <= 0 {
		return "", "", false
	}
	before := s[:colon]
	after := s[colon+1:]
	if strings.Contains(before, "/") || after == "" {
		return "", "", false
	}
	if port, _, hasSlash := strings.Cut(after, "/"); hasSlash && port != "" && isAllDigits(port) {
		return "", "", false
	}
	if _, h, hasUser := strings.Cut(before, "@"); hasUser {
		before = h
	}
	if before == "" {
		return "", "", false
	}
	return before, after, true
}

func isAllDigits(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
