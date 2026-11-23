package sysinfo

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

func TestFixtures(t *testing.T) {
	Root = "testdata"
	t.Cleanup(func() { Root = "/" })

	m, err := Meminfo()
	if err != nil {
		t.Fatal(err)
	}
	if want := (Mem{Total: 16384000 * 1024, Available: 4096000 * 1024, SwapTotal: 8192000 * 1024, SwapFree: 8000000 * 1024}); m != want {
		t.Errorf("Meminfo = %+v, want %+v", m, want)
	}
	if m.Used() != (16384000-4096000)*1024 {
		t.Errorf("Used = %d", m.Used())
	}

	sw, err := Swaps()
	if err != nil {
		t.Fatal(err)
	}
	if want := (Swap{ZramBytes: 4194304 * 1024, DiskBytes: 3997696 * 1024}); sw != want || sw.Total() != 8192000*1024 {
		t.Errorf("Swaps = %+v (total %d), want %+v", sw, sw.Total(), want)
	}

	p, err := Pressure("memory")
	if err != nil {
		t.Fatal(err)
	}
	if p != 12.5 {
		t.Errorf("Pressure = %v, want 12.5", p)
	}

	if n, err := OOMKills(); err != nil || n != 3 {
		t.Errorf("OOMKills = %d, %v; want 3", n, err)
	}

	var c CPU
	busy, err := c.Busy()
	if err != nil {
		t.Fatal(err)
	}
	// 200 busy (user+system) of 1000; iowait counts as idle.
	if busy != 0.2 {
		t.Errorf("Busy = %v, want 0.2", busy)
	}
	if again, _ := c.Busy(); again != 0 {
		t.Errorf("Busy with no time passed = %v, want 0", again)
	}
}

// Commands in a slice under a capped user.slice get its memory, when it is
// fuller than the host's.
func TestMemoryOfTheTightestDomain(t *testing.T) {
	Root = "testdata"
	t.Cleanup(func() { Root = "/" })
	m, err := Memory("/user.slice/user-hotbed.slice")
	if err != nil {
		t.Fatal(err)
	}
	want := Mem{Total: 8 << 30, Available: 1 << 30, SwapTotal: 8192000 * 1024, SwapFree: 8000000 * 1024, Domain: "/user.slice"}
	if m != want {
		t.Errorf("Memory = %+v, want %+v", m, want)
	}
	host, err := Memory("/hotbed.slice")
	if err != nil {
		t.Fatal(err)
	}
	if host.Domain != "" || host.Total != 16384000*1024 {
		t.Errorf("Memory outside any limit = %+v, want the host's", host)
	}
}

func TestParseSwaps(t *testing.T) {
	for _, tc := range []struct {
		name    string
		input   string
		want    Swap
		wantErr bool
	}{
		{
			name:  "empty",
			input: "",
			want:  Swap{},
		},
		{
			name:  "header only",
			input: "Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n",
			want:  Swap{},
		},
		{
			name: "zram only (asahimbp)",
			input: strings.Join([]string{
				"Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority",
				"/dev/zram0                              partition\t8388592\t\t3602992\t\t100",
				"",
			}, "\n"),
			want: Swap{ZramBytes: 8388592 * 1024},
		},
		{
			name: "disk only (vm1 and swapfile)",
			input: strings.Join([]string{
				"Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority",
				"/dev/sdc                                partition\t25165820\t2733244\t\t-1",
				"/swapfile                               file\t\t4194300\t\t0\t\t-2",
			}, "\n"),
			want: Swap{DiskBytes: (25165820 + 4194300) * 1024},
		},
		{
			name: "mixed zram and disk (ubu1 plus second zram)",
			input: strings.Join([]string{
				"Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority",
				"/dev/zram0                              partition\t14527996\t3230604\t\t100",
				"/dev/zram12                             partition\t1048576\t\t0\t\t100",
				"/dev/nvme0n1p3                          partition\t8388604\t\t0\t\t-1",
			}, "\n"),
			want: Swap{ZramBytes: (14527996 + 1048576) * 1024, DiskBytes: 8388604 * 1024},
		},
		{
			name: "non-zram names containing zram count as disk",
			input: strings.Join([]string{
				"Filename\tType\tSize\tUsed\tPriority",
				"/dev/zram-backup\tpartition\t1024\t0\t-1",
				"/swap/zram0\tfile\t2048\t0\t-2",
			}, "\n"),
			want: Swap{DiskBytes: (1024 + 2048) * 1024},
		},
		{
			name:    "malformed too few fields",
			input:   "Filename Type Size Used Priority\n/dev/zram0 partition 1024\n",
			wantErr: true,
		},
		{
			name:    "malformed non-numeric size",
			input:   "Filename Type Size Used Priority\n/dev/zram0 partition abc 0 100\n",
			wantErr: true,
		},
		{
			name:    "malformed negative size",
			input:   "Filename Type Size Used Priority\n/dev/zram0 partition -1024 0 100\n",
			wantErr: true,
		},
		{
			name:    "malformed overflow size",
			input:   "Filename Type Size Used Priority\n/dev/zram0 partition 9223372036854775807 0 100\n",
			wantErr: true,
		},
		{
			name: "overflow summing zram rows",
			input: strings.Join([]string{
				"Filename Type Size Used Priority",
				"/dev/zram0 partition " + strconv.FormatInt((math.MaxInt64/1024)/2+1, 10) + " 0 100",
				"/dev/zram1 partition " + strconv.FormatInt((math.MaxInt64/1024)/2+1, 10) + " 0 100",
			}, "\n"),
			wantErr: true,
		},
		{
			name: "overflow summing disk rows",
			input: strings.Join([]string{
				"Filename Type Size Used Priority",
				"/dev/sda1 partition " + strconv.FormatInt((math.MaxInt64/1024)/2+1, 10) + " 0 -1",
				"/dev/sdb1 partition " + strconv.FormatInt((math.MaxInt64/1024)/2+1, 10) + " 0 -2",
			}, "\n"),
			wantErr: true,
		},
		{
			name: "overflow summing across zram and disk",
			input: strings.Join([]string{
				"Filename Type Size Used Priority",
				"/dev/zram0 partition " + strconv.FormatInt((math.MaxInt64/1024)/2+1, 10) + " 0 100",
				"/dev/nvme0n1p3 partition " + strconv.FormatInt((math.MaxInt64/1024)/2+1, 10) + " 0 -1",
			}, "\n"),
			wantErr: true,
		},
		{
			name: "max non-overflowing sum across zram and disk",
			input: strings.Join([]string{
				"Filename Type Size Used Priority",
				"/dev/zram0 partition " + strconv.FormatInt((math.MaxInt64/1024)/2+1, 10) + " 0 100",
				"/dev/nvme0n1p3 partition " + strconv.FormatInt((math.MaxInt64/1024)/2, 10) + " 0 -1",
			}, "\n"),
			want: Swap{
				ZramBytes: ((math.MaxInt64 / 1024) / 2 + 1) * 1024,
				DiskBytes: ((math.MaxInt64 / 1024) / 2) * 1024,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseSwaps(strings.NewReader(tc.input))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseSwaps succeeded (%+v), want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSwaps: %v", err)
			}
			if got != tc.want {
				t.Errorf("ParseSwaps = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestSwapsMissingFile(t *testing.T) {
	Root = t.TempDir()
	t.Cleanup(func() { Root = "/" })

	got, err := Swaps()
	if err != nil {
		t.Fatalf("Swaps on missing file: %v", err)
	}
	if got != (Swap{}) {
		t.Errorf("Swaps on missing file = %+v, want zero", got)
	}
}
