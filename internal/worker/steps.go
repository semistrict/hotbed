package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/shell"
)

// A project's steps are like database migrations: each base remembers which
// version of every step it ran, and the next generation, forked from it,
// runs only what is new or changed, plus the every-pull steps when one of
// the project's repos moved.

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

// heads is the commit each of a project's repos is to be checked out at,
// by repo ID.
type heads map[string]string

// at says whether base b has exactly these repos checked out at these
// commits.
func (h heads) at(b *pb.Base) bool {
	if b == nil || len(b.Repos) != len(h) {
		return false
	}
	for _, r := range b.Repos {
		if h[r.Repo] != r.Commit {
			return false
		}
	}
	return true
}

// toRun is which of p's steps a generation forked from prev (nil: from
// nothing) must run to be at h.
func toRun(p *pb.HostedProject, prev *pb.Base, h heads) []*pb.Step {
	have := applied(prev)
	pulled := !h.at(prev)
	var out []*pb.Step
	for _, s := range p.Steps {
		if have[s.Name] != s.Hash || (s.EveryPull && pulled) {
			out = append(out, s)
		}
	}
	return out
}

// recipe fingerprints what a base is built for: the shared base image key,
// each repo's commit, the steps and the scratch epoch. Two freshens with one
// recipe do the same thing.
func recipe(p *pb.HostedProject, h heads, baseImgKey string) string {
	sum := sha256.New()
	fmt.Fprintf(sum, "baseimg=%s\n", baseImgKey)
	for _, r := range p.Repos {
		fmt.Fprintf(sum, "%s %s\n", r.Id, h[r.Id])
	}
	fmt.Fprintf(sum, "%d\n", p.ScratchEpoch)
	for _, s := range p.Steps {
		fmt.Fprintf(sum, "%s %s %t\n", s.Name, s.Hash, s.EveryPull)
	}
	return hex.EncodeToString(sum.Sum(nil))[:16]
}

// plannedSteps is how a generation's steps stand before its freshen: what
// it inherits from prev, and what it is to run (not ok until it has).
func plannedSteps(p *pb.HostedProject, prev *pb.Base, run []*pb.Step) []*pb.AppliedStep {
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
	for _, s := range p.Steps {
		if running[s.Name] {
			out = append(out, &pb.AppliedStep{Name: s.Name, Hash: s.Hash, Ran: true})
		} else {
			out = append(out, &pb.AppliedStep{Name: s.Name, Hash: have[s.Name], Ok: true, Seconds: took[s.Name]})
		}
	}
	return out
}

// stepsScript runs steps in order in /work, each in its own bash with
// -eo pipefail, printing markers that say which ran, how long each took,
// and how far the freshen is.
func stepsScript(run []*pb.Step) string {
	var b strings.Builder
	for i, s := range run {
		fmt.Fprintf(&b, "echo %s\n", shell.Quote(fmt.Sprintf("::hotbed:: step %d/%d %s", i+1, len(run), s.Name)))
		// EPOCHREALTIME without its separator is microseconds.
		b.WriteString("t0=${EPOCHREALTIME//[!0-9]/}\n")
		fmt.Fprintf(&b, "bash -eo pipefail -c %s\n", shell.Quote(s.Script))
		fmt.Fprintf(&b, "printf '%%s %%d\\n' %s \"$(( ${EPOCHREALTIME//[!0-9]/} - t0 ))\"\n", shell.Quote("::hotbed:: step-done "+s.Name))
	}
	b.WriteString("echo '::hotbed:: steps done'\n")
	return b.String()
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
