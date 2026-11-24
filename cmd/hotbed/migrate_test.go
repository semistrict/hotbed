package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/semistrict/hotbed/internal/controller"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

func TestAdminMigrateCLI(t *testing.T) {
	targetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/admin/import" {
			http.NotFound(w, r)
			return
		}
		var payload struct {
			ClusterID string `json:"clusterId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":               true,
			"clusterId":        payload.ClusterID,
			"controllerNodeId": "cf:" + payload.ClusterID,
			"importedTables":   2,
			"importedRows":     3,
			"workerTokens":     map[string]string{"n1": "hbw_tok"},
		})
	}))
	defer targetSrv.Close()

	c := newClusterWith(t, clusterOpts{
		server: func(s *controller.ServerConfig) {
			s.GitAuthHTTP = targetSrv.Client()
		},
	})

	var res pb.MigrateControllerResponse
	c.ok(&res, "admin", "migrate", targetSrv.URL, "--token", "owner-tok")
	if res.TargetController != targetSrv.URL || res.ImportedTables != 2 || res.MintedWorkerTokens != 1 {
		t.Fatalf("admin migrate result = %+v", &res)
	}
}

func TestControllerAuthToken(t *testing.T) {
	dir := t.TempDir()
	tokFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokFile, []byte("  hbo_file_token \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	orig := defaultHotbedDir
	defaultHotbedDir = func() (string, error) { return dir, nil }
	t.Cleanup(func() { defaultHotbedDir = orig })

	t.Setenv("HOTBED_TOKEN", "")
	if got := controllerAuthToken("ubu1:7120"); got != "" {
		t.Fatalf("tailnet controller got token %q, want empty", got)
	}
	if got := controllerAuthToken("http://127.0.0.1:7120"); got != "" {
		t.Fatalf("plaintext http controller got token %q, want empty", got)
	}
	if got := controllerAuthToken("https://ctrl.example.workers.dev"); got != "hbo_file_token" {
		t.Fatalf("https controller got token %q, want hbo_file_token", got)
	}
	t.Setenv("HOTBED_TOKEN", "hbo_env_token")
	if got := controllerAuthToken("https://ctrl.example.workers.dev"); got != "hbo_env_token" {
		t.Fatalf("env override got token %q, want hbo_env_token", got)
	}
}
