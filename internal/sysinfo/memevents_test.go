package sysinfo

import "testing"

func TestParseMemoryEventsOOM(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{
			name: "zero counts",
			raw:  "low 0\nhigh 12\nmax 0\noom 0\noom_kill 0\noom_group_kill 0\n",
			want: false,
		},
		{
			name: "oom_kill recorded",
			raw:  "low 0\nhigh 0\nmax 0\noom 1\noom_kill 1\noom_group_kill 0\n",
			want: true,
		},
		{
			name: "oom_group_kill recorded",
			raw:  "oom 1\noom_kill 0\noom_group_kill 2\n",
			want: true,
		},
		{
			name: "empty",
			raw:  "",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseMemoryEventsOOM(tc.raw); got != tc.want {
				t.Fatalf("ParseMemoryEventsOOM = %v, want %v", got, tc.want)
			}
		})
	}
}
