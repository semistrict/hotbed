package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/worker"
)

func TestWorkerConfigCLI(t *testing.T) {
	c := newFleet(t, 1)
	kernelSHA := sha256.Sum256([]byte("test-vmlinux"))
	kernelSHAHex := hex.EncodeToString(kernelSHA[:])

	// 1. Set cluster-wide defaults via stdin JSON.
	var defOut struct {
		Revision       int64  `json:"revision,string"`
		DefaultRuntime string `json:"defaultRuntime"`
		VM             struct {
			VCPUs          int32  `json:"vcpus"`
			MemMiB         int32  `json:"memMib"`
			DAXWindowBytes string `json:"daxWindowBytes"`
			CacheType      string `json:"cacheType"`
			Kernels        map[string]struct {
				URL     string `json:"url"`
				SHA256  string `json:"sha256"`
				Version string `json:"version"`
			} `json:"kernels"`
		} `json:"vm"`
		Freshen struct {
			MaxDutyCycle               float64 `json:"maxDutyCycle"`
			MinIntervalSeconds         int32   `json:"minIntervalSeconds"`
			MaxCooldownSeconds         int32   `json:"maxCooldownSeconds"`
			FailureCooldownSeconds     int32   `json:"failureCooldownSeconds"`
			MaxOptionalDurationSeconds int32   `json:"maxOptionalDurationSeconds"`
			IdleDelaySeconds           int32   `json:"idleDelaySeconds"`
		} `json:"freshen"`
	}
	c.okIn(
		`{"defaultRuntime":"vm","vm":{"vcpus":8,"memMib":12288,"daxWindowBytes":"17179869184","cacheType":"Unsafe","kernels":{"amd64":{"url":"https://example.com/vmlinux","sha256":"`+kernelSHAHex+`","version":"cladefs-6.12-kvm"}}},"freshen":{"maxDutyCycle":0.05,"minIntervalSeconds":1800,"maxCooldownSeconds":21600,"failureCooldownSeconds":900,"maxOptionalDurationSeconds":1800,"idleDelaySeconds":15}}`,
		&defOut, "config", "set",
	)
	if defOut.Revision != 1 || defOut.DefaultRuntime != "vm" || defOut.VM.VCPUs != 8 || defOut.VM.MemMiB != 12288 {
		t.Fatalf("config set defaults = %+v", defOut)
	}
	if defOut.Freshen.MaxDutyCycle != 0.05 || defOut.Freshen.MinIntervalSeconds != 1800 || defOut.Freshen.IdleDelaySeconds != 15 {
		t.Fatalf("config set defaults freshen = %+v", defOut.Freshen)
	}
	if defOut.VM.Kernels["amd64"].SHA256 != kernelSHAHex {
		t.Fatalf("kernel sha = %q, want %q", defOut.VM.Kernels["amd64"].SHA256, kernelSHAHex)
	}

	// 2. Partial merge into defaults keeps existing kernel while updating vcpus.
	c.okIn(`{"vm":{"vcpus":12}}`, &defOut, "config", "set", "--if-revision", "1")
	if defOut.Revision != 2 || defOut.VM.VCPUs != 12 || defOut.VM.MemMiB != 12288 || defOut.VM.Kernels["amd64"].SHA256 != kernelSHAHex {
		t.Fatalf("merged defaults = %+v", defOut)
	}

	// 3. Stale --if-revision fails with CONFLICT.
	if _, e := c.failsIn(`{"vm":{"vcpus":16}}`, "config", "set", "--if-revision", "1"); e == nil || e.Code != api.CodeConflict {
		t.Fatalf("stale if-revision = %+v, want CONFLICT", e)
	}

	// 4. Per-node override for ubu1 merges over cluster defaults in `config show ubu1`.
	c.okIn(`{"vm":{"enabled":true,"memMib":16384,"cladefsPeerAddr":"100.73.156.10:9185"},"freshen":{"maxDutyCycle":0.08}}`, nil, "config", "set", "ubu1")

	var showOut struct {
		Defaults struct {
			Revision int64 `json:"revision,string"`
		} `json:"defaults"`
		Nodes []struct {
			Node string `json:"node"`
		} `json:"nodes"`
		Effective struct {
			Node           string `json:"node"`
			DefaultRuntime string `json:"defaultRuntime"`
			VM             struct {
				Enabled         bool   `json:"enabled"`
				VCPUs           int32  `json:"vcpus"`
				MemMiB          int32  `json:"memMib"`
				CladefsPeerAddr string `json:"cladefsPeerAddr"`
				Kernels         map[string]struct {
					SHA256 string `json:"sha256"`
				} `json:"kernels"`
			} `json:"vm"`
			Freshen struct {
				MaxDutyCycle       float64 `json:"maxDutyCycle"`
				MinIntervalSeconds int32   `json:"minIntervalSeconds"`
			} `json:"freshen"`
		} `json:"effective"`
	}
	c.ok(&showOut, "config", "show", "ubu1")
	if showOut.Effective.Node != "ubu1" ||
		showOut.Effective.DefaultRuntime != "vm" ||
		!showOut.Effective.VM.Enabled ||
		showOut.Effective.VM.VCPUs != 12 ||
		showOut.Effective.VM.MemMiB != 16384 ||
		showOut.Effective.VM.CladefsPeerAddr != "100.73.156.10:9185" ||
		showOut.Effective.VM.Kernels["amd64"].SHA256 != kernelSHAHex ||
		showOut.Effective.Freshen.MaxDutyCycle != 0.08 ||
		showOut.Effective.Freshen.MinIntervalSeconds != 1800 {
		t.Fatalf("config show ubu1 effective = %+v", showOut.Effective)
	}

	// 5. Plain text formatting works for `config show`.
	if code, out, stderr := c.hotbed("config", "show"); code != 0 || !strings.Contains(out, "defaults") || !strings.Contains(out, "ubu1") {
		t.Fatalf("config show text: code=%d out=%q stderr=%q", code, out, stderr)
	}

	// 6. Delete per-node override and cluster defaults.
	c.ok(nil, "config", "rm", "ubu1")
	c.ok(nil, "config", "rm", "--defaults")
	if _, e := c.fails("config", "rm", "ubu1"); e == nil || e.Code != api.CodeNotFound {
		t.Fatalf("second config rm ubu1 = %+v, want NOT_FOUND", e)
	}
}

func TestSystemUpgradeCLI(t *testing.T) {
	payload := []byte("cladefsd-release-binary")
	sumBytes := sha256.Sum256(payload)
	sumHex := hex.EncodeToString(sumBytes[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cladefsd" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)

	binDir := t.TempDir()
	cladefsdBin := filepath.Join(binDir, "cladefsd")
	if err := os.WriteFile(cladefsdBin, []byte("old-cladefsd"), 0o755); err != nil {
		t.Fatal(err)
	}

	var restarts atomic.Int32
	c := newCluster(t, func(cfg *worker.EngineConfig) {
		cfg.Artifacts = &worker.ArtifactStore{WorkerDir: cfg.Dir, DownloadClient: srv.Client()}
		cfg.SystemUpgradeOps = &worker.SystemUpgradeOps{
			RestartService: func(ctx context.Context, service string) error {
				restarts.Add(1)
				return nil
			},
			Reconnect: func(ctx context.Context) (uint64, error) {
				return 11, nil
			},
		}
	})

	// Configure cluster-wide system.cladefsd artifact for both supported arches
	// (fake.NewSystem reports arm64 regardless of host GOARCH).
	artJSON := `{"url":"` + srv.URL + `/cladefsd","sha256":"` + sumHex + `","version":"cladefs-test"}`
	c.okIn(
		`{"system":{"cladefsd":{"amd64":`+artJSON+`,"arm64":`+artJSON+`},"cladefsdBin":"`+cladefsdBin+`"}}`,
		nil, "config", "set",
	)

	var out struct {
		Nodes []struct {
			Node               string `json:"node"`
			Arch               string `json:"arch"`
			Status             string `json:"status"`
			CladefsdUpdated    bool   `json:"cladefsdUpdated"`
			RestartedCladefsd  bool   `json:"restartedCladefsd"`
			CladefsIncarnation string `json:"cladefsIncarnation"`
		} `json:"nodes"`
	}

	// 1. Dry-run reports pending upgrade without touching the binary or restarting.
	c.ok(&out, "system", "upgrade", "--dry-run")
	if len(out.Nodes) != 1 || out.Nodes[0].Status != "dry-run" || !out.Nodes[0].CladefsdUpdated {
		t.Fatalf("dry-run out = %+v", out)
	}
	if restarts.Load() != 0 {
		t.Fatalf("dry-run restarted cladefsd %d times", restarts.Load())
	}

	// 2. Real upgrade installs binary, restarts cladefsd, and reconnects.
	c.ok(&out, "system", "upgrade")
	if len(out.Nodes) != 1 || out.Nodes[0].Status != "upgraded" || !out.Nodes[0].CladefsdUpdated || !out.Nodes[0].RestartedCladefsd || out.Nodes[0].CladefsIncarnation != "11" {
		t.Fatalf("upgrade out = %+v", out)
	}
	if restarts.Load() != 1 {
		t.Fatalf("restarts = %d, want 1", restarts.Load())
	}
	if got, err := os.ReadFile(cladefsdBin); err != nil || string(got) != string(payload) {
		t.Fatalf("installed cladefsd = (%q, %v), want %q", got, err, payload)
	}

	// 3. Re-running without --force reports up-to-date and does not restart again.
	if code, text, stderr := c.hotbed("system", "upgrade", "asahimbp"); code != 0 || !strings.Contains(text, "up-to-date") {
		t.Fatalf("second system upgrade text: code=%d out=%q stderr=%q", code, text, stderr)
	}
	if restarts.Load() != 1 {
		t.Fatalf("restarts after up-to-date = %d, want 1", restarts.Load())
	}

	// 4. Verify `system upgrade` is marked `waits: true` so the CLI uses the
	// no-header-timeout stream client for large binary downloads + rolling restarts.
	schema, err := apiSchema()
	if err != nil {
		t.Fatalf("apiSchema: %v", err)
	}
	found := false
	for _, cmd := range schema.Commands {
		if cmd.Name() == "system upgrade" {
			found = true
			if !cmd.Waits {
				t.Fatalf("system upgrade command has Waits=false, want true")
			}
		}
	}
	if !found {
		t.Fatal("system upgrade command not found in apiSchema")
	}
}
