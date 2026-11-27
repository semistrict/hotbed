package gitauth

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

type ghAPIHTTPRequest struct {
	Method  string   `json:"method"`
	Path    string   `json:"path"`
	Host    string   `json:"host"`
	Repo    string   `json:"repo"`
	Headers []string `json:"headers"`
	Body    string   `json:"body"`
}

type ghAPIHTTPResponse struct {
	StatusCode int      `json:"statusCode"`
	Headers    []string `json:"headers"`
	Body       string   `json:"body"`
}

type ghAuthStatusItem struct {
	Name        string   `json:"name"`
	Host        string   `json:"host"`
	Username    string   `json:"username"`
	Repos       []string `json:"repos"`
	Refreshable bool     `json:"refreshable"`
	ExpiresAt   int64    `json:"expiresAt"`
}

type ghAuthStatusResponse struct {
	Credentials []ghAuthStatusItem `json:"credentials"`
}

func (s *Server) handleGHAPI(w http.ResponseWriter, r *http.Request) {
	var req ghAPIHTTPRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("decode gh api request: %v", err))
		return
	}
	resp, err := s.provider.ProxyGitHubAPI(r.Context(), &pb.ProxyGitHubAPIRequest{
		Method:  req.Method,
		Path:    req.Path,
		Host:    req.Host,
		Repo:    req.Repo,
		Headers: req.Headers,
		Body:    req.Body,
	})
	if err != nil {
		ae := api.FromConnect(err)
		status := http.StatusBadGateway
		switch ae.Code {
		case api.CodeNotFound:
			status = http.StatusNotFound
		case api.CodeNotAuthorized:
			status = http.StatusForbidden
		case api.CodeInvalidArgs:
			status = http.StatusBadRequest
		}
		msg := ae.Message
		if ae.Suggestion != "" {
			msg += " (" + ae.Suggestion + ")"
		}
		writeJSONError(w, status, msg)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ghAPIHTTPResponse{
		StatusCode: int(resp.StatusCode),
		Headers:    resp.Headers,
		Body:       resp.Body,
	})
}

func (s *Server) handleGHAuthStatus(w http.ResponseWriter, r *http.Request) {
	host := strings.TrimSpace(r.URL.Query().Get("host"))
	resp, err := s.provider.ListGitCredentials(r.Context(), &pb.ListGitCredentialsRequest{Host: host})
	if err != nil {
		ae := api.FromConnect(err)
		writeJSONError(w, http.StatusBadGateway, ae.Message)
		return
	}
	out := ghAuthStatusResponse{
		Credentials: make([]ghAuthStatusItem, 0, len(resp.GetCredentials())),
	}
	for _, c := range resp.GetCredentials() {
		out.Credentials = append(out.Credentials, ghAuthStatusItem{
			Name:        c.Name,
			Host:        c.Host,
			Username:    c.Username,
			Repos:       c.Repos,
			Refreshable: c.Refreshable,
			ExpiresAt:   c.ExpiresAt,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func writeGHShim(dir string) (string, error) {
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", fmt.Errorf("create git-auth bin dir: %w", err)
	}
	if err := os.Chmod(binDir, 0o755); err != nil {
		return "", fmt.Errorf("chmod git-auth bin dir: %w", err)
	}
	ghPath := filepath.Join(binDir, "gh")
	if err := os.WriteFile(ghPath, []byte(ghShimScript), 0o755); err != nil {
		return "", fmt.Errorf("write gh shim: %w", err)
	}
	if err := os.Chmod(ghPath, 0o755); err != nil {
		return "", fmt.Errorf("chmod gh shim: %w", err)
	}
	return binDir, nil
}

// ghShimScript is a lightweight `gh` CLI placed first on PATH for every worker
// command. Instead of holding GitHub credentials on the worker, it delegates
// REST and GraphQL calls over the worker's local Unix socket to the controller's
// ProxyGitHubAPI and ListGitCredentials RPCs.
//
//go:embed ghshim.py
var ghShimScript string
