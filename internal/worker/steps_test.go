package worker

import (
	"fmt"
	"testing"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

func TestToRunIsMigrationsPlusEveryPull(t *testing.T) {
	r := &pb.Repo{Steps: []*pb.Step{
		{Name: "tool", Hash: "t2"},
		{Name: "deps", Hash: "d1", EveryPull: true},
		{Name: "new", Hash: "n1"},
	}}
	prev := &pb.Base{Commit: "c1", Steps: []*pb.AppliedStep{
		{Name: "tool", Hash: "t1", Ok: true},
		{Name: "deps", Hash: "d1", Ok: true},
	}}
	names := func(steps []*pb.Step) string {
		var out []string
		for _, s := range steps {
			out = append(out, s.Name)
		}
		return fmt.Sprint(out)
	}
	if got := names(toRun(r, prev, "c1")); got != "[tool new]" {
		t.Errorf("same commit: %s, want the changed and the new step", got)
	}
	if got := names(toRun(r, prev, "c2")); got != "[tool deps new]" {
		t.Errorf("after a pull: %s, want every-pull too", got)
	}
	if got := names(toRun(r, nil, "c1")); got != "[tool deps new]" {
		t.Errorf("from nothing: %s, want all", got)
	}
}

func TestSettleStepsFromALogThatFailed(t *testing.T) {
	steps := []*pb.AppliedStep{
		{Name: "tool", Hash: "t1", Ok: true}, // inherited
		{Name: "deps", Hash: "d1", Ran: true},
		{Name: "build", Hash: "b1", Ran: true},
		{Name: "later", Hash: "l1", Ran: true},
	}
	log := []byte("::hotbed:: step 1/3 deps\nfetching\n::hotbed:: step-done deps 1500000\n::hotbed:: step 2/3 build\nerror: no\n")
	got := settleSteps(steps, log)
	want := "[tool ok=true ran=false 0s deps ok=true ran=true 1.5s build ok=false ran=true 0s]"
	var desc []string
	for _, s := range got {
		desc = append(desc, fmt.Sprintf("%s ok=%t ran=%t %gs", s.Name, s.Ok, s.Ran, s.Seconds))
	}
	if fmt.Sprint(desc) != want {
		t.Errorf("settled %v, want %s", desc, want)
	}
	if f := failedStep(&pb.Base{Steps: got}); f != "build" {
		t.Errorf("failed step %q, want build", f)
	}
	if p := stepProgress("/nonexistent"); p != 0 {
		t.Errorf("progress without a log = %v", p)
	}
}
