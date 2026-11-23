package setup

import (
	"slices"
	"testing"
)

// worker init inside the macOS VM delegates storage, join, trim timer, and
// systemd setup to `hotbed --json worker init --zfs-device`; since hotbed
// prints plain text by default (board 7259), it has to ask for --json.
func TestVMWorkerInitArgs(t *testing.T) {
	cfg := VMConfig{
		Controller:     "ubu1:7120",
		StateDir:       "/var/lib/hotbed",
		User:           "ramon",
		Hostname:       "mac-worker",
		MemoryBytes:    12 << 30,
		Slice:          "work.slice",
		UnitProperties: []string{"DeviceAllow=/dev/kvm"},
	}
	got := VMWorkerInitArgs(cfg, false)
	want := []string{
		"sudo", "/usr/local/bin/hotbed", "--json", "worker", "init",
		"ubu1:7120",
		"--dir", "/var/lib/hotbed",
		"--user", "ramon",
		"--zfs-device", zfsDiskByID,
		"--fstrim",
		"--zfs-arc-max", "3221225472",
		"--hostname", "mac-worker",
		"--slice", "work.slice",
		"--unit-property", "DeviceAllow=/dev/kvm",
	}
	if !slices.Equal(got, want) {
		t.Errorf("VMWorkerInitArgs(noStart=false) =\n%q\nwant\n%q", got, want)
	}

	gotNoStart := VMWorkerInitArgs(cfg, true)
	wantNoStart := append(slices.Clone(want), "--no-start")
	if !slices.Equal(gotNoStart, wantNoStart) {
		t.Errorf("VMWorkerInitArgs(noStart=true) =\n%q\nwant\n%q", gotNoStart, wantNoStart)
	}
}

// A new VM takes a quarter of the Mac's RAM, in whole GiB from 4 to 16: the
// Mac holds all of it while the VM runs, so half of a 48 GiB Mac starved it.
func TestDefaultVMMemory(t *testing.T) {
	for _, tc := range []struct{ host, want int64 }{
		{8 << 30, 4 << 30},
		{18 << 30, 4 << 30},
		{24 << 30, 6 << 30},
		{36 << 30, 9 << 30},
		{48 << 30, 12 << 30},
		{50 << 30, 12 << 30},
		{64 << 30, 16 << 30},
		{128 << 30, 16 << 30},
	} {
		if got := DefaultVMMemory(tc.host); got != tc.want {
			t.Errorf("DefaultVMMemory(%s) = %s, want %s", FormatBytes(tc.host), FormatBytes(got), FormatBytes(tc.want))
		}
	}
}
