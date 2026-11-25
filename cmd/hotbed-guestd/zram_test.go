package main

import "testing"

func TestParseMeminfoMemTotal(t *testing.T) {
	raw := "MemTotal:        8151296 kB\nMemFree:         7123456 kB\nSwapTotal:             0 kB\n"
	if got, want := parseMeminfoMemTotal(raw), int64(8151296*1024); got != want {
		t.Fatalf("parseMeminfoMemTotal = %d, want %d", got, want)
	}
	if got := parseMeminfoMemTotal("MemFree: 100 kB\n"); got != 0 {
		t.Fatalf("missing MemTotal = %d, want 0", got)
	}
	if got := parseMeminfoMemTotal("MemTotal: invalid kB\n"); got != 0 {
		t.Fatalf("invalid MemTotal = %d, want 0", got)
	}
}

func TestComputeZramDiskSize(t *testing.T) {
	if got := computeZramDiskSize(0); got != 0 {
		t.Fatalf("computeZramDiskSize(0) = %d, want 0", got)
	}
	if got := computeZramDiskSize(32 << 20); got != 64<<20 {
		t.Fatalf("computeZramDiskSize(32 MiB) = %d, want 64 MiB floor", got)
	}
	if got, want := computeZramDiskSize(8<<30+123), int64(8<<30); got != want {
		t.Fatalf("computeZramDiskSize(8 GiB + 123) = %d, want %d", got, want)
	}
}

func TestSelectZramCompAlgorithm(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"lzo [lzo-rle] lz4 lz4hc 842 zstd\n", "zstd"},
		{"[lzo-rle] lz4 lzo\n", "lz4"},
		{"lzo [lzo-rle]\n", "lzo-rle"},
		{"[lzo]\n", "lzo"},
		{"[zstd]\n", "zstd"},
		{"deflate 842\n", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := selectZramCompAlgorithm(tc.raw); got != tc.want {
			t.Errorf("selectZramCompAlgorithm(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestSwapActiveForDevice(t *testing.T) {
	swaps := "Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n/dev/zram0                              partition\t8388604\t\t0\t\t100\n"
	if !swapActiveForDevice(swaps, "/dev/zram0") {
		t.Fatal("expected /dev/zram0 to be detected as active")
	}
	if swapActiveForDevice(swaps, "/dev/zram1") {
		t.Fatal("did not expect /dev/zram1 to be detected as active")
	}
	if swapActiveForDevice("Filename Type Size Used Priority\n", "/dev/zram0") {
		t.Fatal("empty swaps table should not match")
	}
}
