package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/shell"
)

// A repo's steps are like database migrations: each base remembers which
// version of every step it ran, and the next generation, forked from it,
// runs only what is new or changed, plus the every-pull steps when the
// branch moved.

// applied is what a base has run successfully, by step name: the script
// hashes it carries, ran there or inherited.
func applied(b *pb.Base) map[string]string {
	out := map[string]string{}
	if b == nil {
		return out
	}
	for _, s := range b.Steps {
		if s.Ok {
			out[s.Name] = s.Hash
		}
	}
	return out
}

// toRun is which of r's steps a generation forked from prev (nil: from
// nothing) must run to be at commit.
func toRun(r *pb.Repo, prev *pb.Base, commit string) []*pb.Step {
	have := applied(prev)
	pulled := prev == nil || prev.Commit != commit
	var out []*pb.Step
	for _, s := range r.Steps {
		if have[s.Name] != s.Hash || (s.EveryPull && pulled) {
			out = append(out, s)
		}
	}
	return out
}

// recipe fingerprints what a base is built for: the commit, the steps and
// the scratch epoch. Two freshens with one recipe do the same thing.
func recipe(r *pb.Repo, commit string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%d\n", commit, r.ScratchEpoch)
	for _, s := range r.Steps {
		fmt.Fprintf(h, "%s %s %t\n", s.Name, s.Hash, s.EveryPull)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// plannedSteps is how a generation's steps stand before its freshen: what
// it inherits from prev, and what it is to run (not ok until it has).
func plannedSteps(r *pb.Repo, prev *pb.Base, run []*pb.Step) []*pb.AppliedStep {
	running := map[string]bool{}
	for _, s := range run {
		running[s.Name] = true
	}
	took := map[string]float64{} // how long inherited steps took where they ran
	if prev != nil {
		for _, a := range prev.Steps {
			took[a.Name] = a.Seconds
		}
	}
	have := applied(prev)
	var out []*pb.AppliedStep
	for _, s := range r.Steps {
		if running[s.Name] {
			out = append(out, &pb.AppliedStep{Name: s.Name, Hash: s.Hash, Ran: true})
		} else {
			out = append(out, &pb.AppliedStep{Name: s.Name, Hash: have[s.Name], Ok: true, Seconds: took[s.Name]})
		}
	}
	return out
}

var (
	stepStart = regexp.MustCompile(`(?m)^::hotbed:: step (\d+)/(\d+) (\S+)$`)
	stepDone  = regexp.MustCompile(`(?m)^::hotbed:: step-done (\S+) (\d+)$`)
)

// stepsScript runs steps in order in the checkout, each in its own bash with
// -eo pipefail, printing markers that say which ran, how long each took,
// and how far the freshen is.
func stepsScript(run []*pb.Step) string {
	var b strings.Builder
	for i, s := range run {
		fmt.Fprintf(&b, "echo %s\n", shell.Quote(fmt.Sprintf("::hotbed:: step %d/%d %s", i+1, len(run), s.Name)))
		// EPOCHREALTIME without its separator is microseconds.
		b.WriteString("t0=${EPOCHREALTIME//[!0-9]/}\n")
		fmt.Fprintf(&b, "bash -eo pipefail -c %s\n", shell.Quote(s.Script))
		fmt.Fprintf(&b, "echo \"::hotbed:: step-done %s $(( ${EPOCHREALTIME//[!0-9]/} - t0 ))\"\n", s.Name)
	}
	b.WriteString("echo '::hotbed:: steps done'\n")
	return b.String()
}

// settleSteps marks the steps a freshen ran as done, with their times, from
// its log; the one it started and did not finish failed. Steps after it
// never ran, so the base does not have them and they are left out.
func settleSteps(steps []*pb.AppliedStep, log []byte) []*pb.AppliedStep {
	done := map[string]float64{}
	for _, m := range stepDone.FindAllSubmatch(log, -1) {
		us, _ := strconv.ParseInt(string(m[2]), 10, 64)
		done[string(m[1])] = float64(us) / 1e6
	}
	started := map[string]bool{}
	for _, m := range stepStart.FindAllSubmatch(log, -1) {
		started[string(m[3])] = true
	}
	var kept []*pb.AppliedStep
	for _, s := range steps {
		if s.Ran {
			secs, ok := done[s.Name]
			s.Ok, s.Seconds = ok, secs
			if !ok && !started[s.Name] {
				continue // never reached
			}
		}
		kept = append(kept, s)
	}
	return kept
}

// failedStep is the step a broken base failed at, if one did.
func failedStep(b *pb.Base) string {
	for _, s := range b.Steps {
		if s.Ran && !s.Ok {
			return s.Name
		}
	}
	return ""
}

// stepProgress reads how far a freshen got from its log: step k of n
// starting means k-1 are done.
func stepProgress(path string) float64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	m := stepStart.FindAllSubmatch(data, -1)
	if len(m) == 0 {
		return 0
	}
	last := m[len(m)-1]
	k, _ := strconv.Atoi(string(last[1]))
	n, _ := strconv.Atoi(string(last[2]))
	if n == 0 {
		return 0
	}
	return float64(k-1) / float64(n)
}
