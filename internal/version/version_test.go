package version

import "testing"

func TestParseTag(t *testing.T) {
	for tag, want := range map[string]int64{"build-42": 42, "build-1": 1, "v1.0": 0, "build-x": 0, "build-0": 0, "build--3": 0} {
		if got := ParseTag(tag); got != want {
			t.Errorf("ParseTag(%q) = %d, want %d", tag, got, want)
		}
	}
	if got := Tag(42); got != "build-42" {
		t.Errorf("Tag(42) = %q", got)
	}
}

// A binary built by CI says its build; one built by hand is a dev build,
// build 0, whatever it says of its commit.
func TestStrings(t *testing.T) {
	for _, tc := range []struct {
		build, commit string
		n             int64
		want          string
	}{
		{"42", "0123456789abcdef", 42, "build 42 (0123456789ab)"},
		{"", "0123456789abcdef", 0, "dev (0123456789ab)"},
		{"", "", 0, "dev"},
		{"nonsense", "", 0, "dev"},
	} {
		if n, s := parse(tc.build), describe(tc.build, tc.commit); n != tc.n || s != tc.want {
			t.Errorf("build %q commit %q: %d %q, want %d %q", tc.build, tc.commit, n, s, tc.n, tc.want)
		}
	}
}
