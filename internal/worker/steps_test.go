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
	repos := []*pb.Repo{{Id: "app", Url: "https://example.com/app.git"}, {Id: "lib", Url: "https://example.com/lib.git"}}
	got := freshenScript(repos, heads{"app": "a1", "lib": "l1"}, []string{"app", "gone", "../etc"}, []*pb.Step{{Name: "build", Script: "make"}})
	for _, want := range []string{
		"mkdir -p '/work'\nrm -rf '/work/gone'\n",
		"cd '/work'\nif [ ! -d 'app'/.git ]; then git clone --quiet --filter=blob:none --no-checkout -- 'https://example.com/app.git' 'app'; fi\ncd 'app'\n",
		"git checkout --quiet --detach --force 'a1'\n",
		"cd '/work'\nif [ ! -d 'lib'/.git ]; then git clone --quiet --filter=blob:none --no-checkout -- 'https://example.com/lib.git' 'lib'; fi\ncd 'lib'\n",
		"git checkout --quiet --detach --force 'l1'\n",
		"cd '/work'\necho '::hotbed:: step 1/1 build'\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("freshen script lacks %q:\n%s", want, got)
		}
	}
	for _, never := range []string{"rm -rf '/work/app'", "rm -rf '/work/../etc'"} {
		if strings.Contains(got, never) {
			t.Errorf("freshen script runs %q:\n%s", never, got)
		}
	}
}

// A repo URL is whatever the project's owner wrote: the freshen script runs
// it past bash as a literal, never as $(...) or `...`, and an incremental
// freshen takes out the checkout of a repo the project dropped. Every VM,
// cladefs and host base builds with this one script.
func TestFreshenScriptRunsMetacharacterURLsLiterallyAndDropsRemovedRepos(t *testing.T) {
	tmp := t.TempDir()
	marker1, marker2 := filepath.Join(tmp, "pwn1"), filepath.Join(tmp, "pwn2")
	origin := filepath.Join(tmp, fmt.Sprintf("origin $(touch %s) `touch %s` 'q\"", marker1, marker2))
	runGitTest(t, tmp, "init", "-q", "-b", "main", origin)
	if err := os.WriteFile(filepath.Join(origin, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, origin, "add", ".")
	runGitTest(t, origin, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "one")
	head := runGitTest(t, origin, "rev-parse", "HEAD")

	work := filepath.Join(tmp, "work")
	run := func(repos []*pb.Repo, h heads, had []string) string {
		t.Helper()
		// The script names /work; aim it at the test's own.
		script := strings.ReplaceAll(freshenScript(repos, h, had, nil), "'"+WorkPath, "'"+work)
		out, err := exec.Command("bash", "-c", script).CombinedOutput()
		if err != nil {
			t.Fatalf("freshen script failed: %v\nscript:\n%s\noutput:\n%s", err, script, out)
		}
		return string(out)
	}
	both := []*pb.Repo{{Id: "app", Url: origin}, {Id: "gone", Url: origin}}
	out := run(both, heads{"app": head, "gone": head}, nil)
	for _, m := range []string{marker1, marker2} {
		if _, err := os.Stat(m); !os.IsNotExist(err) {
			t.Fatalf("the repo URL ran as shell: %s exists (%v)", m, err)
		}
	}
	for _, want := range []string{"::hotbed-head:: app " + head + "\n", "::hotbed-head:: gone " + head + "\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if got := runGitTest(t, filepath.Join(work, "app"), "rev-parse", "HEAD"); got != head {
		t.Fatalf("app checked out %s, want %s", got, head)
	}

	run(both[:1], heads{"app": head}, []string{"app", "gone"})
	if _, err := os.Stat(filepath.Join(work, "gone")); !os.IsNotExist(err) {
		t.Fatalf("dropped repo's checkout still there (%v)", err)
	}
	if _, err := os.Stat(filepath.Join(work, "app", "f.txt")); err != nil {
		t.Fatalf("held repo's checkout gone: %v", err)
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
