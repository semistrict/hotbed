package worker

import (
	"slices"
	"testing"
)

func TestMountsUnderADirectory(t *testing.T) {
	info := []byte(`22 1 0:21 / / rw - ext4 /dev/sda1 rw
40 22 0:50 / /scratch/tmp/abc rw - fuse.cladefs cladefs rw
41 40 0:51 / /scratch/tmp/abc/inner\040dir rw - tmpfs tmpfs rw
42 22 0:52 / /scratch/tmp/abcd rw - tmpfs tmpfs rw
`)
	got := parseMountsUnder(info, "/scratch/tmp/abc")
	want := []string{"/scratch/tmp/abc/inner dir", "/scratch/tmp/abc"}
	if !slices.Equal(got, want) {
		t.Errorf("mounts under = %q, want %q (deepest first, not abcd)", got, want)
	}
}
