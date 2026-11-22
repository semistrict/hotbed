package bench

import (
	"slices"
	"testing"
)

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
