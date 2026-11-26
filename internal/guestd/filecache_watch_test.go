package guestd

import (
	"sync/atomic"
	"testing"
	"time"

	guestdv1 "github.com/semistrict/hotbed/internal/gen/guestd/v1"
)

func TestParseMeminfoCleanFileBytes(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int64
	}{
		{
			name: "typical build guest excludes shmem and dirty",
			raw: "" +
				"MemTotal:       12582912 kB\n" +
				"Cached:          4194304 kB\n" +
				"Active(anon):    2097152 kB\n" +
				"Inactive(anon):  1048576 kB\n" +
				"Active(file):    1048576 kB\n" +
				"Inactive(file):  2097152 kB\n" +
				"Shmem:           1048576 kB\n" +
				"Dirty:            131072 kB\n" +
				"Writeback:         65536 kB\n",
			want: (1048576 + 2097152 - 131072 - 65536) * 1024,
		},
		{
			name: "clamps negative when dirty exceeds file",
			raw:  "Active(file): 100 kB\nInactive(file): 50 kB\nDirty: 200 kB\n",
			want: 0,
		},
		{
			name: "empty returns zero",
			raw:  "",
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseMeminfoCleanFileBytes(tc.raw); got != tc.want {
				t.Fatalf("parseMeminfoCleanFileBytes = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestFileCacheWatcherThresholdCooldownAndSync(t *testing.T) {
	srv, err := NewServer(ServerConfig{
		WorkDir:              t.TempDir(),
		StateDir:             t.TempDir(),
		RunDir:               t.TempDir(),
		FileCacheThreshold:   1 << 30,
		FileCacheMinCooldown: 30 * time.Second,
		FileCacheMaxCooldown: 5 * time.Minute,
		FileCacheMinFreed:    64 << 20,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	var clean atomic.Int64
	var syncs, drops, compacts atomic.Int32
	srv.cleanFileBytes = func() int64 { return clean.Load() }
	srv.syncFS = func() { syncs.Add(1) }
	srv.dropCaches = func() error {
		drops.Add(1)
		// Simulate dropping most clean file cache down to 200 MiB.
		clean.Store(200 << 20)
		return nil
	}
	srv.compactMem = func() error {
		compacts.Add(1)
		return nil
	}

	w := srv.newFileCacheWatcher()
	t0 := time.Date(2026, 11, 26, 15, 0, 0, 0, time.UTC)

	// Below threshold: no reclaim.
	clean.Store(900 << 20)
	if w.step(t0) {
		t.Fatal("expected no reclaim below threshold")
	}
	if drops.Load() != 0 {
		t.Fatalf("drops = %d, want 0", drops.Load())
	}

	// Active running command + above threshold: drops and compacts, skips sync.
	srv.mu.Lock()
	srv.cmds["c1"] = &managedCmd{id: "c1", state: guestdv1.CommandState_COMMAND_STATE_RUNNING}
	srv.mu.Unlock()

	clean.Store(1500 << 20)
	if !w.step(t0.Add(10 * time.Second)) {
		t.Fatal("expected reclaim above threshold")
	}
	if drops.Load() != 1 || compacts.Load() != 1 || syncs.Load() != 0 {
		t.Fatalf("drops=%d compacts=%d syncs=%d, want 1/1/0", drops.Load(), compacts.Load(), syncs.Load())
	}

	// Within 30s cooldown: even if cache spikes again, wait for cooldown.
	clean.Store(2000 << 20)
	if w.step(t0.Add(35 * time.Second)) {
		t.Fatal("expected cooldown to suppress reclaim at +25s")
	}
	if drops.Load() != 1 {
		t.Fatalf("drops = %d, want 1 during cooldown", drops.Load())
	}

	// Idle VM after cooldown expires: syncs, drops, compacts.
	srv.mu.Lock()
	srv.cmds["c1"].state = guestdv1.CommandState_COMMAND_STATE_EXITED
	srv.mu.Unlock()

	if !w.step(t0.Add(40 * time.Second)) {
		t.Fatal("expected reclaim after cooldown elapsed")
	}
	if drops.Load() != 2 || compacts.Load() != 2 || syncs.Load() != 1 {
		t.Fatalf("drops=%d compacts=%d syncs=%d, want 2/2/1", drops.Load(), compacts.Load(), syncs.Load())
	}
}

func TestFileCacheWatcherPinnedBackoffAndGrowthOverride(t *testing.T) {
	srv, err := NewServer(ServerConfig{
		WorkDir:              t.TempDir(),
		StateDir:             t.TempDir(),
		RunDir:               t.TempDir(),
		FileCacheThreshold:   1 << 30,
		FileCacheMinCooldown: 30 * time.Second,
		FileCacheMaxCooldown: 2 * time.Minute,
		FileCacheMinFreed:    64 << 20,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.mu.Lock()
	srv.cmds["c1"] = &managedCmd{id: "c1", state: guestdv1.CommandState_COMMAND_STATE_RUNNING}
	srv.mu.Unlock()

	var clean atomic.Int64
	var dropFreed atomic.Int64
	var drops, compacts atomic.Int32
	srv.cleanFileBytes = func() int64 { return clean.Load() }
	srv.syncFS = func() {}
	srv.dropCaches = func() error {
		drops.Add(1)
		clean.Add(-dropFreed.Load())
		return nil
	}
	srv.compactMem = func() error {
		compacts.Add(1)
		return nil
	}

	w := srv.newFileCacheWatcher()
	t0 := time.Date(2026, 11, 26, 15, 0, 0, 0, time.UTC)

	// 1.5 GiB pinned file cache: drop frees only 8 MiB (< 64 MiB) -> skip compact, back off to 60s.
	clean.Store(1536 << 20)
	dropFreed.Store(8 << 20)
	if !w.step(t0) {
		t.Fatal("expected initial reclaim attempt")
	}
	if drops.Load() != 1 || compacts.Load() != 0 {
		t.Fatalf("pinned cache: drops=%d compacts=%d, want 1/0", drops.Load(), compacts.Load())
	}

	// At +35s (past minCooldown=30s, inside backoff=60s) with no growth: suppressed.
	if w.step(t0.Add(35 * time.Second)) {
		t.Fatal("expected backoff to suppress reclaim at +35s")
	}

	// At +40s, if 600 MiB of new clean cache builds up (> 512 MiB override), break backoff early.
	clean.Add(600 << 20)
	dropFreed.Store(600 << 20)
	if !w.step(t0.Add(40 * time.Second)) {
		t.Fatal("expected growth override to trigger reclaim during backoff")
	}
	if drops.Load() != 2 || compacts.Load() != 1 {
		t.Fatalf("after productive reclaim: drops=%d compacts=%d, want 2/1", drops.Load(), compacts.Load())
	}
	if w.backedOff || w.cooldown != 30*time.Second {
		t.Fatalf("expected backoff cleared after productive drop, got backedOff=%v cooldown=%s", w.backedOff, w.cooldown)
	}
}

func TestFileCacheWatcherSkipsWhenReclaimLocked(t *testing.T) {
	srv, err := NewServer(ServerConfig{
		WorkDir:            t.TempDir(),
		StateDir:           t.TempDir(),
		RunDir:             t.TempDir(),
		FileCacheThreshold: 1 << 30,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	var drops atomic.Int32
	srv.cleanFileBytes = func() int64 { return 2 << 30 }
	srv.dropCaches = func() error { drops.Add(1); return nil }

	w := srv.newFileCacheWatcher()
	srv.reclaimMu.Lock()
	defer srv.reclaimMu.Unlock()

	if w.step(time.Now()) {
		t.Fatal("expected step to skip when reclaimMu is already held")
	}
	if drops.Load() != 0 {
		t.Fatalf("drops = %d, want 0", drops.Load())
	}
}
