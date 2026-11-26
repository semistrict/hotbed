package main

import (
	"io"
	"slices"
	"testing"
)

// A flag the command does not know, before "--", is refused: kong would pass
// it, and every flag after it, through as the command, which the args after
// "--" then replace, dropping them unnoticed (an older hotbed given a newer
// flag, such as --keep-idle, ran without it and without the --mem after it).
func TestUnknownFlagBeforeDashDashIsRefused(t *testing.T) {
	for _, args := range [][]string{
		{"run", "--no-such-flag", "--mem", "8G", "--", "true"},
		{"shell", "--no-such-flag", "--", "true"},
	} {
		_, _, _, _, err := parse(args, io.Discard)
		want := "INVALID_ARGS: unknown flag --no-such-flag of hotbed " + args[0] + " (before --)"
		if err == nil || err.Error() != want {
			t.Errorf("parse %q: err %v, want %q", args, err, want)
		}
	}
	if _, _, _, _, err := parse([]string{"run", "make", "--", "-j8"}, io.Discard); err == nil || err.Error() != "INVALID_ARGS: unexpected argument make of hotbed run (before --)" {
		t.Errorf("parse run make -- -j8: err %v", err)
	}

	cmd, _, _, _, err := parse([]string{"run", "--keep-idle", "--mem", "8G", "--", "make", "test"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	run := cmd.(*runCmd)
	if !run.KeepIdle || run.Mem != "8G" || !slices.Equal(run.Args, []string{"make", "test"}) {
		t.Errorf("run --keep-idle --mem 8G -- make test = keepIdle %v, mem %q, args %q", run.KeepIdle, run.Mem, run.Args)
	}
}
