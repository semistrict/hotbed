package cfbundle

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHarnessServesTestEndpointsAndAssetsThenCloses(t *testing.T) {
	assets := t.TempDir()
	if err := os.WriteFile(filepath.Join(assets, "index.html"), []byte("<!doctype html><title>cockpit</title>"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := StartHarness(t.Context(), HarnessOptions{Assets: assets, Token: "hbo_cfbundle_test"})
	if err != nil {
		t.Fatalf("StartHarness: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	if h.Token() != "hbo_cfbundle_test" {
		t.Errorf("Token() = %q, want the --token passed", h.Token())
	}
	if !strings.HasPrefix(h.URL(), "http://127.0.0.1:") {
		t.Errorf("URL() = %q, want http://127.0.0.1:<port>", h.URL())
	}

	var cfg struct {
		ClusterID string `json:"clusterId"`
	}
	if err := h.DoJSON(t.Context(), "cfbundle-test", http.MethodPost, "/__test/config", map[string]any{"allowLocalRepos": true}, &cfg); err != nil {
		t.Fatalf("DoJSON /__test/config: %v", err)
	}
	if cfg.ClusterID == "" {
		t.Errorf("/__test/config returned no clusterId")
	}

	// --assets reaches the Worker's production asset path: an owner's request
	// for a deep link gets the SPA's index.html.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.URL()+"/sessions/tidy-owl", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.Token())
	req.Header.Set("X-Hotbed-Test-DO", "cfbundle-test")
	req.Header.Set("Accept", "text/html")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET deep link: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "<title>cockpit</title>") {
		t.Errorf("GET deep link = %d %q, want 200 with index.html", resp.StatusCode, body)
	}

	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-h.done:
	default:
		t.Errorf("harness process still running after Close")
	}
	if err := h.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if _, err := http.Get(h.URL() + "/healthz"); err == nil {
		t.Errorf("harness still answers at %s after Close", h.URL())
	}
}

// The harness takes its bundle from --bundle, and says so when it is missing:
// the flag used to be silently ignored.
func TestStartHarnessHonorsBundleFlag(t *testing.T) {
	root, err := FindRoot()
	if err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "no-such-bundle")
	_, err = startHarness(t.Context(), root, missing, HarnessOptions{})
	if err == nil {
		t.Fatal("startHarness with a missing --bundle succeeded")
	}
	want := "cf-controller bundle missing at " + filepath.Join(missing, "index.js")
	if !strings.Contains(err.Error(), "exited before it was ready") || !strings.Contains(err.Error(), want) {
		t.Errorf("startHarness error = %v, want an early exit saying %q", err, want)
	}
}

func TestStartHarnessGivesUpWhenNotReady(t *testing.T) {
	root, err := FindRoot()
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := Ensure(root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = startHarness(t.Context(), root, bundle, HarnessOptions{ReadyTimeout: time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "was not ready in 1ms") {
		t.Errorf("startHarness with a 1ms deadline = %v, want a not-ready error", err)
	}
}

func TestHarnessOptionsArgs(t *testing.T) {
	got := HarnessOptions{
		Assets: "/a", Port: 9, Owner: "o", Name: "n", Token: "t", JoinToken: "j", Persist: "/p",
	}.args("/b")
	want := []string{"--bundle", "/b", "--port", "9", "--assets", "/a", "--owner", "o", "--name", "n", "--token", "t", "--join-token", "j", "--persist", "/p"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("args = %v, want %v", got, want)
	}
	if got := (HarnessOptions{}).args("/b"); strings.Join(got, " ") != "--bundle /b" {
		t.Errorf("zero options args = %v, want just --bundle /b", got)
	}
}
