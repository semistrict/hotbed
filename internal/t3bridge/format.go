package t3bridge

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// FormatWakeMessage formats one or more answered/dismissed Hotbed questions
// into a concise, actionable wake message for a T3 Code thread.
func FormatWakeMessage(questions []*pb.Question) string {
	ordered := slices.Clone(questions)
	slices.SortFunc(ordered, func(a, b *pb.Question) int {
		return cmp.Compare(a.GetId(), b.GetId())
	})

	var b strings.Builder
	if len(ordered) == 1 {
		q := ordered[0]
		if q.GetState() == pb.QuestionState_QUESTION_STATE_DISMISSED {
			b.WriteString("[Hotbed] Your Hotbed question was dismissed:\n\n")
		} else {
			b.WriteString("[Hotbed] Your Hotbed question was answered:\n\n")
		}
	} else {
		fmt.Fprintf(&b, "[Hotbed] %d of your Hotbed questions were updated:\n\n", len(ordered))
	}

	ackIDs := make([]string, 0, len(ordered))
	for _, q := range ordered {
		ackIDs = append(ackIDs, strconv.Itoa(int(q.GetId())))
		header := strings.TrimSpace(q.GetPrompt().GetHeader())
		title := strings.TrimSpace(q.GetPrompt().GetQuestion())
		switch {
		case header != "" && title != "":
			fmt.Fprintf(&b, "- **#%d (%s)** — %s\n", q.GetId(), header, title)
		case header != "":
			fmt.Fprintf(&b, "- **#%d (%s)**\n", q.GetId(), header)
		case title != "":
			fmt.Fprintf(&b, "- **#%d** — %s\n", q.GetId(), title)
		default:
			fmt.Fprintf(&b, "- **#%d**\n", q.GetId())
		}

		switch q.GetState() {
		case pb.QuestionState_QUESTION_STATE_DISMISSED:
			reason := strings.TrimSpace(q.GetDismissed().GetReason())
			if reason == "" {
				reason = "(no reason given)"
			}
			fmt.Fprintf(&b, "  - **Dismissed:** %s\n", reason)
		default:
			ans := q.GetAnswer()
			if picked := ans.GetPicked(); len(picked) > 0 {
				fmt.Fprintf(&b, "  - **Picked:** %s\n", strings.Join(picked, ", "))
			}
			if other := strings.TrimSpace(ans.GetOther()); other != "" {
				fmt.Fprintf(&b, "  - **Other:** %s\n", other)
			}
			if notes := strings.TrimSpace(ans.GetNotes()); notes != "" {
				fmt.Fprintf(&b, "  - **Notes:** %s\n", notes)
			}
		}
	}

	b.WriteString("\n")
	if len(ordered) == 1 {
		b.WriteString("Please review and act on this response, then acknowledge it with:\n")
	} else {
		b.WriteString("Please review and act on these responses, then acknowledge them with:\n")
	}
	fmt.Fprintf(&b, "`hotbed question ack %s`\n", strings.Join(ackIDs, " "))
	return b.String()
}
