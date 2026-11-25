//go:build linux

package guestd_test

import (
	"context"
	"testing"
	"time"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/guestd"
)

func TestLinuxRealInspectAndSampleInGuestd(t *testing.T) {
	workDir := t.TempDir()
	// Nil Inspector defaults to real inspect.Linux{}.
	client, _ := startTestGuestd(t, workDir)
	ctx := context.Background()

	st, err := client.StartCommand(ctx, guestd.StartCommandRequest{
		ID:   "cmd-linux-real",
		Argv: []string{"/bin/sh", "-c", "sleep 30"},
		Dir:  workDir,
	})
	if err != nil {
		t.Fatalf("StartCommand: %v", err)
	}
	t.Cleanup(func() { _, _ = client.StopCommand(context.Background(), "cmd-linux-real", 500*time.Millisecond) })

	ins, err := client.InspectCommand(ctx, &pb.InspectCommandRequest{
		Id: "cmd-linux-real",
		Sections: []pb.InspectSection{
			pb.InspectSection_INSPECT_SECTION_PROCS,
			pb.InspectSection_INSPECT_SECTION_FILES,
			pb.InspectSection_INSPECT_SECTION_MAPS,
			pb.InspectSection_INSPECT_SECTION_LIMITS,
		},
	})
	if err != nil {
		t.Fatalf("InspectCommand(real Linux): %v", err)
	}
	if len(ins.Procs) == 0 {
		t.Fatalf("expected live Linux processes in inspect response (mainPID=%d), got 0", st.MainPID)
	}
	foundMain := false
	for _, p := range ins.Procs {
		if int(p.Pid) == st.MainPID {
			foundMain = true
			break
		}
	}
	if !foundMain {
		t.Fatalf("expected main PID %d in inspected procs %+v", st.MainPID, ins.Procs)
	}

	var sample *pb.ProcessSample
	if err := client.SampleCommand(ctx, &pb.SampleCommandRequest{
		Id:              "cmd-linux-real",
		DurationSeconds: 1,
	}, func(ch *pb.SampleCommandChunk) error {
		if ch.Sample != nil {
			sample = ch.Sample
		}
		return nil
	}); err != nil {
		t.Fatalf("SampleCommand(real Linux): %v", err)
	}
	if sample == nil || len(sample.Procs) == 0 || sample.PollCount == 0 {
		t.Fatalf("unexpected real Linux sample: %+v", sample)
	}
}
