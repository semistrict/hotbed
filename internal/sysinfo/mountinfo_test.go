package sysinfo

import (
	"slices"
	"strings"
	"testing"
)

// The mounts under a directory are what mountinfo names below it, decoded,
// once each, deepest first; a sibling sharing its name's prefix is not.
func TestParseMountsUnder(t *testing.T) {
	info := "" +
		"100 1 0:40 / /var/lib/cladefs/mnt rw,nosuid - fuse.cladefs cladefs rw\n" +
		"101 100 0:40 / /var/lib/cladefs/mnt/t0/c1 rw - fuse.cladefs cladefs rw\n" +
		"102 101 0:41 / /var/lib/cladefs/mnt/t0/c1/proc rw - proc proc rw\n" +
		"103 102 0:42 / /var/lib/cladefs/mnt/t0/c1/proc/sys/fs/binfmt_misc rw - autofs systemd-1 rw\n" +
		"104 101 0:43 / /var/lib/cladefs/mnt/t0/c1/tmp/my\\040dir rw - tmpfs tmpfs rw\n" +
		"105 101 0:43 / /var/lib/cladefs/mnt/t0/c1/tmp/my\\040dir rw - tmpfs tmpfs rw\n" +
		"106 100 0:44 / /var/lib/cladefs/mnt/t0/c10/proc rw - proc proc rw\n" +
		"107 101 0:45 / /var/lib/cladefs/mnt/t0/c1/" + strings.Repeat("d", 100<<10) + " rw - tmpfs tmpfs rw" // no newline, past a Scanner's line
	parse := func(dir string, scope MountScope) []string {
		t.Helper()
		got, err := ParseMountsUnder(strings.NewReader(info), dir, scope)
		if err != nil {
			t.Fatalf("ParseMountsUnder(%q): %v", dir, err)
		}
		return got
	}
	beneath := []string{
		"/var/lib/cladefs/mnt/t0/c1/proc/sys/fs/binfmt_misc",
		"/var/lib/cladefs/mnt/t0/c1/tmp/my dir",
		"/var/lib/cladefs/mnt/t0/c1/" + strings.Repeat("d", 100<<10),
		"/var/lib/cladefs/mnt/t0/c1/proc",
	}
	if got := parse("/var/lib/cladefs/mnt/t0/c1/", Beneath); !slices.Equal(got, beneath) {
		t.Errorf("beneath = %q, want %q", got, beneath)
	}
	if got, want := parse("/var/lib/cladefs/mnt/t0/c1", AtOrBeneath), append(slices.Clone(beneath), "/var/lib/cladefs/mnt/t0/c1"); !slices.Equal(got, want) {
		t.Errorf("at or beneath = %q, want %q", got, want)
	}
	for _, dir := range []string{"/", "", "var/lib/cladefs/mnt"} {
		if got := parse(dir, AtOrBeneath); len(got) != 0 {
			t.Errorf("under %q = %q, want none", dir, got)
		}
	}
}

func TestUnescapeMountinfoPath(t *testing.T) {
	for in, want := range map[string]string{
		`/a\040b`:   "/a b",
		`/tab\011x`: "/tab\tx",
		`/nl\012`:   "/nl\n",
		`/bs\134`:   `/bs\`,
		`/not\9xx`:  `/not\9xx`,
		`/big\777`:  `/big\777`,
		`/short\04`: `/short\04`,
	} {
		if got := UnescapeMountinfoPath(in); got != want {
			t.Errorf("UnescapeMountinfoPath(%q) = %q, want %q", in, got, want)
		}
	}
}
