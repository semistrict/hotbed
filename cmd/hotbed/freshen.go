package main

import (
	"fmt"
	"slices"
	"strings"

	"github.com/semistrict/hotbed/internal/apicmd"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

func formatFreshenListReport(res *pb.ListFreshensResponse) string {
	var b strings.Builder
	s := res.GetSummary()
	if s == nil || s.GetTotal() == 0 {
		return "no base freshens in window\n"
	}

	windowLabel := "all history"
	if s.GetWindowSince() > 0 && res.GetNow() > s.GetWindowSince() {
		windowLabel = "last " + fmtFreshenDur(float64(res.GetNow()-s.GetWindowSince())/1000)
	}
	var headParts []string
	headParts = append(headParts, fmt.Sprintf("%d total", s.GetTotal()))
	if s.GetActive() > 0 {
		var actParts []string
		if s.GetQueued() > 0 {
			actParts = append(actParts, fmt.Sprintf("%d queued", s.GetQueued()))
		}
		if s.GetRunning() > 0 {
			actParts = append(actParts, fmt.Sprintf("%d running", s.GetRunning()))
		}
		if s.GetParked() > 0 {
			actParts = append(actParts, fmt.Sprintf("%d parked", s.GetParked()))
		}
		if s.GetFinalizing() > 0 {
			actParts = append(actParts, fmt.Sprintf("%d finalizing", s.GetFinalizing()))
		}
		headParts = append(headParts, fmt.Sprintf("%d active (%s)", s.GetActive(), strings.Join(actParts, ", ")))
	}
	if s.GetSucceeded() > 0 {
		headParts = append(headParts, fmt.Sprintf("%d succeeded", s.GetSucceeded()))
	}
	if s.GetFailed() > 0 {
		headParts = append(headParts, fmt.Sprintf("%d failed", s.GetFailed()))
	}
	if s.GetCancelled() > 0 {
		headParts = append(headParts, fmt.Sprintf("%d cancelled/dropped", s.GetCancelled()))
	}
	headParts = append(headParts, fmt.Sprintf("%d cold / %d incr", s.GetCold(), s.GetIncremental()))
	if s.GetWaitingWorkspaces() > 0 {
		headParts = append(headParts, fmt.Sprintf("%d workspaces waiting", s.GetWaitingWorkspaces()))
	}
	fmt.Fprintf(&b, "Base freshens (%s): %s\n", windowLabel, strings.Join(headParts, " · "))

	var timeParts []string
	if d := s.GetTotalDuration(); d != nil && d.GetCount() > 0 {
		timeParts = append(timeParts, fmt.Sprintf("total p50 %s (p95 %s, max %s)",
			fmtFreshenDur(d.GetP50Seconds()), fmtFreshenDur(d.GetP95Seconds()), fmtFreshenDur(d.GetMaxSeconds())))
	}
	if d := s.GetQueueDuration(); d != nil && d.GetTotalSeconds() >= 1 {
		timeParts = append(timeParts, fmt.Sprintf("queue sum %s (max %s)",
			fmtFreshenDur(d.GetTotalSeconds()), fmtFreshenDur(d.GetMaxSeconds())))
	}
	if d := s.GetPrepDuration(); d != nil && d.GetTotalSeconds() >= 1 {
		timeParts = append(timeParts, fmt.Sprintf("prep p50 %s (max %s)",
			fmtFreshenDur(d.GetP50Seconds()), fmtFreshenDur(d.GetMaxSeconds())))
	}
	if d := s.GetGitDuration(); d != nil && d.GetTotalSeconds() >= 1 {
		timeParts = append(timeParts, fmt.Sprintf("git p50 %s (max %s)",
			fmtFreshenDur(d.GetP50Seconds()), fmtFreshenDur(d.GetMaxSeconds())))
	}
	if d := s.GetStepsDuration(); d != nil && d.GetCount() > 0 {
		timeParts = append(timeParts, fmt.Sprintf("steps p50 %s (max %s)",
			fmtFreshenDur(d.GetP50Seconds()), fmtFreshenDur(d.GetMaxSeconds())))
	}
	if d := s.GetParkedDuration(); d != nil && d.GetTotalSeconds() >= 1 {
		timeParts = append(timeParts, fmt.Sprintf("parked sum %s (%d parks)",
			fmtFreshenDur(d.GetTotalSeconds()), s.GetTotalParks()))
	}
	if d := s.GetFinalizeDuration(); d != nil && d.GetTotalSeconds() >= 1 {
		timeParts = append(timeParts, fmt.Sprintf("finalize p50 %s (max %s)",
			fmtFreshenDur(d.GetP50Seconds()), fmtFreshenDur(d.GetMaxSeconds())))
	}
	if len(timeParts) > 0 {
		fmt.Fprintf(&b, "Timings: %s\n", strings.Join(timeParts, " · "))
	}

	if len(s.GetByNodeProject()) > 0 {
		b.WriteString("\nBy node & project:\n")
		for _, np := range s.GetByNodeProject() {
			var parts []string
			coldInfo := fmt.Sprintf("%d runs", np.GetRuns())
			if np.GetColdRuns() > 0 {
				var reasons []string
				for k, v := range np.GetColdReasons() {
					if k != "" && k != "unknown" {
						reasons = append(reasons, fmt.Sprintf("%d %s", v, k))
					}
				}
				slices.Sort(reasons)
				if len(reasons) > 0 {
					coldInfo += fmt.Sprintf(" (%d cold: %s)", np.GetColdRuns(), strings.Join(reasons, ", "))
				} else {
					coldInfo += fmt.Sprintf(" (%d cold)", np.GetColdRuns())
				}
			}
			parts = append(parts, coldInfo)

			var outcomes []string
			if np.GetSucceeded() > 0 {
				outcomes = append(outcomes, fmt.Sprintf("%d ok", np.GetSucceeded()))
			}
			if np.GetFailed() > 0 {
				outcomes = append(outcomes, fmt.Sprintf("%d fail", np.GetFailed()))
			}
			if np.GetCancelled() > 0 {
				outcomes = append(outcomes, fmt.Sprintf("%d drop/cancel", np.GetCancelled()))
			}
			if len(outcomes) > 0 {
				parts = append(parts, strings.Join(outcomes, " "))
			}
			if np.GetConsecutiveFailures() >= 2 {
				parts = append(parts, fmt.Sprintf("streak=%d fail", np.GetConsecutiveFailures()))
			}
			if np.GetConsecutiveCold() >= 2 {
				parts = append(parts, fmt.Sprintf("streak=%d cold", np.GetConsecutiveCold()))
			}
			if np.GetMaxTotalSeconds() > 0 {
				parts = append(parts, fmt.Sprintf("p50=%s max=%s",
					fmtFreshenDur(np.GetP50TotalSeconds()), fmtFreshenDur(np.GetMaxTotalSeconds())))
			}
			if np.GetTotalQueueSeconds() >= 5 {
				parts = append(parts, fmt.Sprintf("queue=%s", fmtFreshenDur(np.GetTotalQueueSeconds())))
			}
			if np.GetTotalParkedSeconds() >= 1 {
				parts = append(parts, fmt.Sprintf("parked=%s", fmtFreshenDur(np.GetTotalParkedSeconds())))
			}
			if np.GetTotalFinalizeSeconds() >= 2 {
				parts = append(parts, fmt.Sprintf("finalize=%s", fmtFreshenDur(np.GetTotalFinalizeSeconds())))
			}
			if np.GetActiveState() != pb.FreshenState_FRESHEN_STATE_UNSPECIFIED {
				act := fmt.Sprintf("ACTIVE %s %s", enumWord(np.GetActiveState()), fmtFreshenDur(np.GetActiveElapsedSeconds()))
				if np.GetActiveStep() != "" {
					act += " (" + np.GetActiveStep() + ")"
				}
				if np.GetActiveReason() != "" {
					act += " [" + apicmd.Short(70, np.GetActiveReason()) + "]"
				}
				parts = append(parts, act)
			}
			if np.GetWaitingWorkspaces() > 0 {
				parts = append(parts, fmt.Sprintf("%d ws waiting", np.GetWaitingWorkspaces()))
			}
			fmt.Fprintf(&b, "  %-12s %-14s %-7s %s\n",
				np.GetNode(), np.GetProject(), np.GetRuntime(), strings.Join(parts, " · "))
		}
	}

	if len(s.GetSlowestSteps()) > 0 {
		b.WriteString("\nSlowest steps:\n")
		for _, st := range s.GetSlowestSteps() {
			failStr := ""
			if st.GetFailures() > 0 {
				failStr = fmt.Sprintf(", %d fail", st.GetFailures())
			}
			fmt.Fprintf(&b, "  %-34s %2d runs%s · sum %-7s · p50 %-7s · max %s\n",
				st.GetProject()+" / "+st.GetStep(),
				st.GetRuns(), failStr,
				fmtFreshenDur(st.GetTotalSeconds()),
				fmtFreshenDur(st.GetP50Seconds()),
				fmtFreshenDur(st.GetMaxSeconds()))
		}
	}

	if len(res.GetFreshens()) > 0 {
		b.WriteString("\nFreshens (active first, then newest):\n")
		for _, f := range res.GetFreshens() {
			mode := "incr"
			if f.GetCold() {
				mode = "cold"
				if f.GetColdReason() != "" {
					mode += "(" + f.GetColdReason() + ")"
				}
			} else if f.GetFromBaseId() != "" {
				mode += "(" + f.GetFromBaseId() + ")"
			}

			var metrics []string
			if f.GetTotalSeconds() > 0 {
				metrics = append(metrics, "total="+fmtFreshenDur(f.GetTotalSeconds()))
			}
			if f.GetPrepSeconds() >= 1 {
				metrics = append(metrics, "prep="+fmtFreshenDur(f.GetPrepSeconds()))
			}
			if f.GetQueueSeconds() >= 1 {
				metrics = append(metrics, "q="+fmtFreshenDur(f.GetQueueSeconds()))
			}
			if f.GetGitSeconds() >= 1 {
				metrics = append(metrics, "git="+fmtFreshenDur(f.GetGitSeconds()))
			}
			if f.GetStepsSeconds() >= 1 {
				metrics = append(metrics, "steps="+fmtFreshenDur(f.GetStepsSeconds()))
			}
			if f.GetParkedSeconds() >= 1 {
				metrics = append(metrics, fmt.Sprintf("parked=%s(%d)", fmtFreshenDur(f.GetParkedSeconds()), f.GetParks()))
			}
			if f.GetFinalizeSeconds() >= 1 {
				metrics = append(metrics, "fin="+fmtFreshenDur(f.GetFinalizeSeconds()))
			}
			if f.GetMemoryLowBytes() > 0 && f.GetState() == pb.FreshenState_FRESHEN_STATE_QUEUED {
				metrics = append(metrics, fmt.Sprintf("memLow=%.1fG", float64(f.GetMemoryLowBytes())/(1<<30)))
			}
			if f.GetCurrentStep() != "" {
				stepStr := fmt.Sprintf("step=%d/%d %s", f.GetCurrentStepIndex(), f.GetTotalSteps(), f.GetCurrentStep())
				if f.GetCurrentStepSeconds() >= 1 {
					stepStr += fmt.Sprintf("(%s)", fmtFreshenDur(f.GetCurrentStepSeconds()))
				}
				metrics = append(metrics, stepStr)
			}
			if f.GetFailedStep() != "" {
				metrics = append(metrics, "failed="+f.GetFailedStep())
			}
			if len(f.GetWaitingWorkspaces()) > 0 {
				metrics = append(metrics, fmt.Sprintf("%d ws waiting", len(f.GetWaitingWorkspaces())))
			}
			if f.GetReason() != "" {
				metrics = append(metrics, apicmd.Short(80, f.GetReason()))
			}

			fmt.Fprintf(&b, "  %-24s %-10s %-10s %-7s %-10s %-24s %s\n",
				f.GetId(),
				f.GetNode(),
				f.GetProject(),
				f.GetRuntime(),
				enumWord(f.GetState()),
				mode,
				strings.Join(metrics, "  "))
		}
		if res.GetMore() > 0 {
			fmt.Fprintf(&b, "  (+%d older matching freshens not shown: pass --limit 0 for all)\n", res.GetMore())
		}
	}

	return b.String()
}

func formatFreshenShowReport(res *pb.GetFreshenResponse) string {
	f := res.GetFreshen()
	if f == nil {
		return "no freshen found\n"
	}
	var b strings.Builder

	fmt.Fprintf(&b, "Freshen %s  (base %s, %s)\n", f.GetId(), f.GetBaseId(), enumWord(f.GetState()))
	mode := "incremental"
	if f.GetCold() {
		mode = "cold (from nothing)"
		if f.GetColdReason() != "" {
			mode += " — reason: " + f.GetColdReason()
		}
	} else if f.GetFromBaseId() != "" {
		mode += " from " + f.GetFromBaseId()
	}
	fmt.Fprintf(&b, "  Node:     %s (%s)\n", f.GetNode(), f.GetRuntime())
	fmt.Fprintf(&b, "  Project:  %s", f.GetProject())
	if f.GetGeneration() > 0 {
		fmt.Fprintf(&b, " (g%d)", f.GetGeneration())
	}
	b.WriteByte('\n')
	fmt.Fprintf(&b, "  Build:    %s\n", mode)
	if len(f.GetRepos()) > 0 {
		var repos []string
		for _, r := range f.GetRepos() {
			commit := r.GetCommit()
			if len(commit) > 10 {
				commit = commit[:10]
			}
			repos = append(repos, r.GetRepo()+"@"+commit)
		}
		fmt.Fprintf(&b, "  Repos:    %s\n", strings.Join(repos, ", "))
	}
	fmt.Fprintf(&b, "  Created:  %s", apicmd.Clock(f.GetCreatedAt()))
	if f.StartedAt != nil && *f.StartedAt > 0 {
		fmt.Fprintf(&b, "  Started: %s", apicmd.Clock(*f.StartedAt))
	}
	if f.EndedAt != nil && *f.EndedAt > 0 {
		fmt.Fprintf(&b, "  Ended: %s", apicmd.Clock(*f.EndedAt))
	}
	b.WriteByte('\n')
	if f.GetFailedStep() != "" {
		fmt.Fprintf(&b, "  Failed:   step %s\n", f.GetFailedStep())
	}
	if f.GetReason() != "" {
		fmt.Fprintf(&b, "  Reason:   %s\n", f.GetReason())
	}
	if f.GetCurrentStep() != "" {
		fmt.Fprintf(&b, "  Current:  step %d/%d %s (%s elapsed)\n",
			f.GetCurrentStepIndex(), f.GetTotalSteps(), f.GetCurrentStep(), fmtFreshenDur(f.GetCurrentStepSeconds()))
	} else if f.GetCurrentPhase() != "" {
		fmt.Fprintf(&b, "  Current:  %s\n", f.GetCurrentPhase())
	}
	if len(f.GetWaitingWorkspaces()) > 0 {
		fmt.Fprintf(&b, "  Waiting:  %d workspace(s): %s\n",
			len(f.GetWaitingWorkspaces()), strings.Join(f.GetWaitingWorkspaces(), ", "))
	}

	b.WriteString("\nTimings & resources:\n")
	fmt.Fprintf(&b, "  total=%s  prep=%s  queue=%s  run=%s  git=%s  steps=%s  parked=%s (%d parks)  finalize=%s\n",
		fmtFreshenDur(f.GetTotalSeconds()),
		fmtFreshenDur(f.GetPrepSeconds()),
		fmtFreshenDur(f.GetQueueSeconds()),
		fmtFreshenDur(f.GetRunSeconds()),
		fmtFreshenDur(f.GetGitSeconds()),
		fmtFreshenDur(f.GetStepsSeconds()),
		fmtFreshenDur(f.GetParkedSeconds()),
		f.GetParks(),
		fmtFreshenDur(f.GetFinalizeSeconds()))
	var resParts []string
	if f.GetMemoryLowBytes() > 0 {
		resParts = append(resParts, fmt.Sprintf("memory_low=%.2f GiB", float64(f.GetMemoryLowBytes())/(1<<30)))
	}
	if f.GetPeakMemoryBytes() > 0 {
		resParts = append(resParts, fmt.Sprintf("peak_memory=%.2f GiB", float64(f.GetPeakMemoryBytes())/(1<<30)))
	}
	if f.GetSizeBytes() > 0 {
		resParts = append(resParts, fmt.Sprintf("base_size=%.2f GiB", float64(f.GetSizeBytes())/(1<<30)))
	}
	if f.GetUnits() > 0 {
		resParts = append(resParts, fmt.Sprintf("units=%d", f.GetUnits()))
	}
	if f.ExitCode != nil {
		resParts = append(resParts, fmt.Sprintf("exit_code=%d", *f.ExitCode))
	}
	if len(resParts) > 0 {
		fmt.Fprintf(&b, "  %s\n", strings.Join(resParts, "  "))
	}

	if len(f.GetPhases()) > 0 {
		b.WriteString("\nPhases:\n")
		for _, p := range f.GetPhases() {
			detail := ""
			if p.GetDetail() != "" {
				detail = "  (" + p.GetDetail() + ")"
			}
			fmt.Fprintf(&b, "  %-14s %-9s %s%s\n",
				p.GetName(), fmtFreshenDur(p.GetSeconds()), apicmd.Clock(p.GetStartedAt()), detail)
		}
	}

	if len(f.GetSteps()) > 0 {
		b.WriteString("\nSteps:\n")
		for _, st := range f.GetSteps() {
			status := "inherited"
			if st.GetRan() {
				switch {
				case st.GetOk():
					status = "ok"
				case f.GetCurrentStep() == st.GetName():
					status = "running"
				case f.GetFailedStep() == st.GetName():
					status = "FAILED"
				default:
					status = "pending"
				}
			}
			durStr := "-"
			if st.GetSeconds() > 0 {
				durStr = fmtFreshenDur(st.GetSeconds())
			} else if status == "running" && f.GetCurrentStepSeconds() > 0 {
				durStr = fmtFreshenDur(f.GetCurrentStepSeconds()) + "..."
			}
			fmt.Fprintf(&b, "  %-24s %-10s %s\n", st.GetName(), status, durStr)
		}
	}

	if len(f.GetTimeline()) > 0 {
		b.WriteString("\nCommand timeline:\n")
		for _, ev := range f.GetTimeline() {
			fmt.Fprintf(&b, "  %s  %-10s %s\n", apicmd.Clock(ev.GetAt()), ev.GetWhat(), ev.GetDetail())
		}
	}

	if adm := res.GetNodeAdmission(); adm != nil {
		b.WriteString("\nNode admission:\n")
		if adm.GetHolding() {
			fmt.Fprintf(&b, "  HOLDING: %s\n", adm.GetReason())
		} else {
			b.WriteString("  starting new work\n")
		}
		for _, sig := range adm.GetSignals() {
			valStr := fmt.Sprintf("%.0f%%", sig.GetValue())
			limStr := fmt.Sprintf("%.0f%%", sig.GetLimit())
			if sig.GetUnit() == "bytes" {
				valStr = fmt.Sprintf("%.1f GiB", sig.GetValue()/(1<<30))
				limStr = fmt.Sprintf("%.1f GiB", sig.GetLimit()/(1<<30))
			}
			holdTag := ""
			if sig.GetHolding() {
				holdTag = "  HOLDING"
			}
			extra := ""
			if sig.GetDetail() != "" {
				extra = "  (" + sig.GetDetail() + ")"
			}
			fmt.Fprintf(&b, "  %-20s %s / %s%s%s\n", sig.GetName(), valStr, limStr, holdTag, extra)
		}
	}

	if len(res.GetRelatedEvents()) > 0 {
		b.WriteString("\nRelated events:\n")
		for _, ev := range res.GetRelatedEvents() {
			fmt.Fprintf(&b, "  %s  %-7s %s\n", apicmd.Clock(ev.GetAt()), ev.GetKind(), apicmd.Short(120, ev.GetText()))
		}
	}

	return b.String()
}

func fmtFreshenDur(secs float64) string {
	if secs <= 0 {
		return "0s"
	}
	if secs < 1 {
		return fmt.Sprintf("%dms", int(secs*1000))
	}
	if secs < 60 {
		return fmt.Sprintf("%.1fs", secs)
	}
	total := int(secs + 0.5)
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	}
	return fmt.Sprintf("%dm%02ds", m, s)
}
