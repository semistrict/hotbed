package guestd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseMeminfoSwapTotal(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int64
	}{
		{
			name: "swapless guest",
			raw:  "MemTotal:        4096000 kB\nSwapTotal:             0 kB\nSwapFree:              0 kB\n",
			want: 0,
		},
		{
			name: "swapped guest",
			raw:  "MemTotal:        4096000 kB\nSwapTotal:       1048576 kB\n",
			want: 1048576 * 1024,
		},
		{
			name: "missing field",
			raw:  "MemTotal:        4096000 kB\n",
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseMeminfoSwapTotal(tc.raw); got != tc.want {
				t.Fatalf("parseMeminfoSwapTotal = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestPlanReclaimSwaplessVsSwapped(t *testing.T) {
	t.Run("swapless guest with only anon skips memory.reclaim", func(t *testing.T) {
		cg := t.TempDir()
		stat := "anon 536870912\nfile 0\nslab_reclaimable 0\n"
		if err := os.WriteFile(filepath.Join(cg, "memory.stat"), []byte(stat), 0o644); err != nil {
			t.Fatal(err)
		}
		if payload, ok := planReclaim(cg, 512<<20, 0); ok {
			t.Fatalf("expected skip on swapless cgroup with 0 file/slab, got payload %q", payload)
		}
	})

	t.Run("swapless guest caps to reclaimable file+slab with swappiness=0", func(t *testing.T) {
		cg := t.TempDir()
		stat := "anon 536870912\nfile 12582912\nslab_reclaimable 4194304\n"
		if err := os.WriteFile(filepath.Join(cg, "memory.stat"), []byte(stat), 0o644); err != nil {
			t.Fatal(err)
		}
		payload, ok := planReclaim(cg, 512<<20, 0)
		if !ok || payload != "16777216 swappiness=0" {
			t.Fatalf("planReclaim = (%q, %v), want (\"16777216 swappiness=0\", true)", payload, ok)
		}
		// Smaller requested bytes clamps below available file cache.
		payload2, ok2 := planReclaim(cg, 4<<20, 0)
		if !ok2 || payload2 != "4194304 swappiness=0" {
			t.Fatalf("planReclaim(4MiB) = (%q, %v), want (\"4194304 swappiness=0\", true)", payload2, ok2)
		}
	})

	t.Run("swapless guest falls back to active_file+inactive_file", func(t *testing.T) {
		cg := t.TempDir()
		stat := "anon 1000000\ninactive_file 2048\nactive_file 4096\nslab_reclaimable 1024\n"
		if err := os.WriteFile(filepath.Join(cg, "memory.stat"), []byte(stat), 0o644); err != nil {
			t.Fatal(err)
		}
		payload, ok := planReclaim(cg, 1<<20, 0)
		if !ok || payload != "7168 swappiness=0" {
			t.Fatalf("planReclaim = (%q, %v), want (\"7168 swappiness=0\", true)", payload, ok)
		}
	})

	t.Run("guest with swap requests full target", func(t *testing.T) {
		cg := t.TempDir()
		payload, ok := planReclaim(cg, 512<<20, 2<<30)
		if !ok || payload != "536870912" {
			t.Fatalf("planReclaim(swapped) = (%q, %v), want (\"536870912\", true)", payload, ok)
		}
	})
}
