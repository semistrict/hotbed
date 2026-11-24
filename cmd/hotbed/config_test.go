package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/semistrict/hotbed/internal/api"
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
	}
	c.okIn(
		`{"defaultRuntime":"vm","vm":{"vcpus":8,"memMib":12288,"daxWindowBytes":"17179869184","cacheType":"Unsafe","kernels":{"amd64":{"url":"https://example.com/vmlinux","sha256":"`+kernelSHAHex+`","version":"cladefs-6.12-kvm"}}}}`,
		&defOut, "config", "set",
	)
	if defOut.Revision != 1 || defOut.DefaultRuntime != "vm" || defOut.VM.VCPUs != 8 || defOut.VM.MemMiB != 12288 {
		t.Fatalf("config set defaults = %+v", defOut)
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
	c.okIn(`{"vm":{"enabled":true,"memMib":16384,"cladefsPeerAddr":"100.73.156.10:9185"}}`, nil, "config", "set", "ubu1")

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
		} `json:"effective"`
	}
	c.ok(&showOut, "config", "show", "ubu1")
	if showOut.Effective.Node != "ubu1" ||
		showOut.Effective.DefaultRuntime != "vm" ||
		!showOut.Effective.VM.Enabled ||
		showOut.Effective.VM.VCPUs != 12 ||
		showOut.Effective.VM.MemMiB != 16384 ||
		showOut.Effective.VM.CladefsPeerAddr != "100.73.156.10:9185" ||
		showOut.Effective.VM.Kernels["amd64"].SHA256 != kernelSHAHex {
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
