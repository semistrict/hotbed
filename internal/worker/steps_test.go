package worker

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

func TestToRunIsMigrationsPlusEveryPull(t *testing.T) {
	p := &pb.HostedProject{
		Repos: []*pb.Repo{{Id: "app"}, {Id: "lib"}},
		Steps: []*pb.Step{
			{Name: "tool", Hash: "t2"},
			{Name: "deps", Hash: "d1", EveryPull: true},
			{Name: "new", Hash: "n1"},
		},
	}
	prev := &pb.Base{
		Repos: []*pb.BaseRepo{{Repo: "app", Commit: "a1"}, {Repo: "lib", Commit: "l1"}},
		Steps: []*pb.AppliedStep{
			{Name: "tool", Hash: "t1", Ok: true},
			{Name: "deps", Hash: "d1", Ok: true},
		},
	}
	names := func(steps []*pb.Step) string {
		var out []string
		for _, s := range steps {
			out = append(out, s.Name)
		}
		return fmt.Sprint(out)
	}
	if got := names(toRun(p, prev, heads{"app": "a1", "lib": "l1"})); got != "[tool new]" {
		t.Errorf("same commits: %s, want the changed and the new step", got)
	}
	if got := names(toRun(p, prev, heads{"app": "a1", "lib": "l2"})); got != "[tool deps new]" {
		t.Errorf("after one repo's pull: %s, want every-pull too", got)
	}
	if got := names(toRun(p, prev, heads{"app": "a1", "lib": "l1", "more": "m1"})); got != "[tool deps new]" {
		t.Errorf("with a repo added: %s, want every-pull too", got)
	}
	if got := names(toRun(p, nil, heads{"app": "a1", "lib": "l1"})); got != "[tool deps new]" {
		t.Errorf("from nothing: %s, want all", got)
	}
}

// A recipe changes with any repo's commit or the shared base image key, so a
// freshen that failed for one set of inputs is tried again once any moves.
func TestRecipeCoversEveryRepo(t *testing.T) {
	p := &pb.HostedProject{Repos: []*pb.Repo{{Id: "app"}, {Id: "lib"}}, Steps: []*pb.Step{{Name: "build", Hash: "b1"}}}
	base := recipe(p, heads{"app": "a1", "lib": "l1"}, "baseimg-v1")
	if again := recipe(p, heads{"app": "a1", "lib": "l1"}, "baseimg-v1"); again != base {
		t.Errorf("same commits gave %s and %s", base, again)
	}
	for _, h := range []heads{{"app": "a2", "lib": "l1"}, {"app": "a1", "lib": "l2"}} {
		if r := recipe(p, h, "baseimg-v1"); r == base {
			t.Errorf("commits %v gave the recipe of a1/l1", h)
		}
	}
	for _, key := range []string{"baseimg-v2", ""} {
		if r := recipe(p, heads{"app": "a1", "lib": "l1"}, key); r == base {
			t.Errorf("base image key %q gave the same recipe as baseimg-v1", key)
		}
	}
}

// The freshen checks out every repo side by side in /work, takes out the
// checkouts of repos the project no longer holds, and runs the steps in /work.
func TestFreshenScriptChecksOutEveryRepoInWork(t *testing.T) {
	p := &pb.HostedProject{Repos: []*pb.Repo{{Id: "app", Url: "https://example.com/app.git"}, {Id: "lib", Url: "https://example.com/lib.git"}}}
	prev := &pb.Base{Repos: []*pb.BaseRepo{{Repo: "app"}, {Repo: "gone"}}}
	got := freshenScript(p, heads{"app": "a1", "lib": "l1"}, prev, []*pb.Step{{Name: "build", Script: "make"}})
	for _, want := range []string{
		"rm -rf '/work/gone'\n",
		"cd /work\nif [ ! -d 'app'/.git ]; then git clone --quiet --filter=blob:none --no-checkout -- 'https://example.com/app.git' 'app'; fi\ncd 'app'\n",
		"git checkout --quiet --detach --force 'a1'\n",
		"cd /work\nif [ ! -d 'lib'/.git ]; then git clone --quiet --filter=blob:none --no-checkout -- 'https://example.com/lib.git' 'lib'; fi\ncd 'lib'\n",
		"git checkout --quiet --detach --force 'l1'\n",
		"cd /work\necho '::hotbed:: step 1/1 build'\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("freshen script lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "rm -rf '/work/app'") {
		t.Errorf("freshen script removes a checkout the project holds:\n%s", got)
	}
}

func TestSettleStepsFromALogThatFailed(t *testing.T) {
	steps := []*pb.AppliedStep{
		{Name: "tool", Hash: "t1", Ok: true}, // inherited
		{Name: "deps", Hash: "d1", Ran: true},
		{Name: "build", Hash: "b1", Ran: true},
		{Name: "later", Hash: "l1", Ran: true},
	}
	o := outputOf(t, "::hotbed:: step 1/3 deps\nfetching\n::hotbed:: step-done deps 1500000\n::hotbed:: step 2/3 build\nerror: no")
	if p := o.progress(); p != 1.0/3 {
		t.Errorf("progress = %v, want 1/3: step 2 of 3 started, so 1 is done", p)
	}
	got := o.settle(steps)
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
	if p := newOutput(filepath.Join(t.TempDir(), "none.log")).progress(); p != 0 {
		t.Errorf("progress without a log = %v", p)
	}
}

// Markers a long log dropped do not undo steps: the freshen runs its steps
// in order and stops at the first failure, so those before the last one it
// started are done, and all are once it says its steps are done.
func TestSettleStepsWhoseMarkersWereDropped(t *testing.T) {
	planned := func() []*pb.AppliedStep {
		return []*pb.AppliedStep{
			{Name: "tool", Hash: "t1", Ok: true}, // inherited
			{Name: "a", Hash: "a1", Ran: true},
			{Name: "b", Hash: "b1", Ran: true},
			{Name: "c", Hash: "c1", Ran: true},
		}
	}
	describe := func(steps []*pb.AppliedStep) string {
		var desc []string
		for _, s := range steps {
			desc = append(desc, fmt.Sprintf("%s ok=%t %gs", s.Name, s.Ok, s.Seconds))
		}
		return fmt.Sprint(desc)
	}
	failedAtC := outputOf(t, "[hotbed: 7.0 MiB of output dropped here]\n::hotbed:: step-done b 2000000\n::hotbed:: step 3/3 c\nerror: no")
	if got, want := describe(failedAtC.settle(planned())), "[tool ok=true 0s a ok=true 0s b ok=true 2s c ok=false 0s]"; got != want {
		t.Errorf("failed at c: %s, want %s", got, want)
	}
	allDone := outputOf(t, "[hotbed: 7.0 MiB of output dropped here]\n::hotbed:: steps done\n")
	if got, want := describe(allDone.settle(planned())), "[tool ok=true 0s a ok=true 0s b ok=true 0s c ok=true 0s]"; got != want {
		t.Errorf("all done: %s, want %s", got, want)
	}
}

// Step names containing shell metacharacters must be safely quoted in
// stepsScript so bash never evaluates command substitutions, quotes, or
// variable expansions during base setup (board 8001).
func TestStepsScriptQuotesMetacharacterStepNames(t *testing.T) {
	markerDir := t.TempDir()
	marker1 := filepath.Join(markerDir, "pwn1")
	marker2 := filepath.Join(markerDir, "pwn2")
	marker3 := filepath.Join(markerDir, "pwn3")

	evilName := fmt.Sprintf(`bad$(touch %s)`+"`touch %s`"+`";touch %s;echo '$HOME`, marker1, marker2, marker3)
	script := stepsScript([]*pb.Step{
		{Name: "build", Script: "true"},
		{Name: evilName, Script: "true"},
	})

	cmd := exec.Command("bash", "-euo", "pipefail", "-c", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash failed: %v\nscript:\n%s\noutput:\n%s", err, script, out)
	}
	for _, m := range []string{marker1, marker2, marker3} {
		if _, err := os.Stat(m); !os.IsNotExist(err) {
			t.Fatalf("shell injection executed: marker %s exists (err=%v)\nscript:\n%s", m, err, script)
		}
	}
	stdout := string(out)
	if !strings.Contains(stdout, "::hotbed:: step 2/2 "+evilName+"\n") {
		t.Errorf("stdout missing literal step start for evil name:\n%s", stdout)
	}
	if !strings.Contains(stdout, "::hotbed:: step-done "+evilName+" ") {
		t.Errorf("stdout missing literal step-done for evil name:\n%s", stdout)
	}
	// Normal step marker still parses cleanly through output.settle.
	o := outputOf(t, stdout)
	settled := o.settle([]*pb.AppliedStep{{Name: "build", Hash: "h1", Ran: true}})
	if len(settled) != 1 || !settled[0].Ok {
		t.Errorf("normal step failed to settle from stepsScript output: %+v\noutput:\n%s", settled, stdout)
	}
}
