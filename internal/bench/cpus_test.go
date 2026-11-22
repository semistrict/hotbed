package bench

import (
	"slices"
	"testing"
)

func TestParseCPUs(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want CPUs
	}{
		{"4-7,12-15", CPUs{4, 5, 6, 7, 12, 13, 14, 15}},
		{"7-10", CPUs{7, 8, 9, 10}},
		{"0", CPUs{0}},
		{"3,1,2-2", CPUs{1, 2, 3}},
		{"0-3\n", CPUs{0, 1, 2, 3}},
		{"", nil},
	} {
		got, err := ParseCPUs(tc.in)
		if err != nil {
			t.Fatalf("ParseCPUs(%q): %v", tc.in, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("ParseCPUs(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"4-", "a", "7-4", "1,,2", "-1"} {
		if got, err := ParseCPUs(bad); err == nil {
			t.Errorf("ParseCPUs(%q) = %v, want an error", bad, got)
		}
	}
}

func TestCPUsString(t *testing.T) {
	for _, tc := range []struct {
		in   CPUs
		want string
	}{
		{CPUs{4, 5, 6, 7, 12, 13, 14, 15}, "4-7,12-15"},
		{CPUs{0, 1, 2, 3, 8, 9, 10, 11}, "0-3,8-11"},
		{CPUs{0, 2}, "0,2"},
		{CPUs{1, 2}, "1-2"},
		{nil, ""},
	} {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("%v.String() = %q, want %q", []int(tc.in), got, tc.want)
		}
	}
}

// Masks are as the kernel's cpumask files take them: 32-bit words, most
// significant first, comma-separated.
func TestCPUsMask(t *testing.T) {
	for _, tc := range []struct {
		in   CPUs
		want string
	}{
		{CPUs{0, 1, 2, 3, 8, 9, 10, 11}, "00000f0f"},
		{CPUs{0, 1, 2, 3, 4, 5, 6}, "0000007f"},
		{CPUs{0, 33}, "00000002,00000001"},
		{CPUs{40}, "00000100,00000000"},
	} {
		if got := tc.in.Mask(); got != tc.want {
			t.Errorf("%v.Mask() = %q, want %q", []int(tc.in), got, tc.want)
		}
	}
}

func TestCPUsSets(t *testing.T) {
	online := CPUs{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	bench := CPUs{7, 8, 9, 10}
	if got := online.Minus(bench); !slices.Equal(got, CPUs{0, 1, 2, 3, 4, 5, 6}) {
		t.Errorf("Minus = %v", got)
	}
	if !bench.Intersects(CPUs{0, 7, 8, 9, 10}) || bench.Intersects(CPUs{1, 2}) {
		t.Error("Intersects is wrong")
	}
}

// ParseMask reads what the kernel writes to a cpumask file, padded or not.
func TestParseMask(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want CPUs
	}{
		{"7ff\n", CPUs{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10}},
		{"ffff", CPUs{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}},
		{"00000f0f", CPUs{0, 1, 2, 3, 8, 9, 10, 11}},
		{"00000002,00000001", CPUs{0, 33}},
		{"0", nil},
	} {
		got, err := ParseMask(tc.in)
		if err != nil {
			t.Fatalf("ParseMask(%q): %v", tc.in, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("ParseMask(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	if _, err := ParseMask("xyz"); err == nil {
		t.Error("ParseMask(xyz) succeeded")
	}
}
