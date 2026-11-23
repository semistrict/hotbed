package controller

import (
	"regexp"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// A reviewer that cannot reach hotbed (one in a network-isolated sandbox)
// ends its report with its findings, as AddFindings takes them, in a fenced
// block its caller passes on with the report:
//
//	```json hotbed-findings
//	{"findings": [...]}
//	```

// findingsInfo is the info string that marks a report's findings block.
const findingsInfo = "json hotbed-findings"

// A fence's opening line (CommonMark): three or more backticks or tildes,
// indented at most three spaces, then its info string; and a closing line,
// the same character at least as many times, alone.
var (
	fenceOpen  = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})(.*)$")
	fenceClose = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})[ \t]*$")
)

const reportExample = "end the report with\n```" + findingsInfo + "\n{\"findings\": [{\"summary\": \"...\", \"failure_scenario\": \"...\", \"severity\": \"important\", \"short_summary\": \"...\", \"file\": \"a.go\", \"line\": 12}]}\n```\n" +
	`({"findings": []} when none survive)`

// reportedFindings is the findings of report's last findings block: a
// fenced block of its own, not one quoted inside another. A report without
// one, or whose last one is not closed or not {"findings": [...]}, is
// refused: a forgotten block is not "none found".
func reportedFindings(report string) ([]*pb.ReportedFinding, error) {
	var (
		body     []string
		found    bool
		fence    string // the open fence's run of backticks or tildes; "" outside one
		findings bool   // the open fence is a findings block
		opened   int    // its line, from 1
		inside   []string
	)
	for i, line := range strings.Split(report, "\n") {
		line = strings.TrimRight(line, "\r")
		if fence == "" {
			m := fenceOpen.FindStringSubmatch(line)
			if m == nil || (m[1][0] == '`' && strings.Contains(m[2], "`")) {
				continue
			}
			fence, opened, inside = m[1], i+1, nil
			findings = strings.Join(strings.Fields(m[2]), " ") == findingsInfo
			continue
		}
		if m := fenceClose.FindStringSubmatch(line); m != nil && m[1][0] == fence[0] && len(m[1]) >= len(fence) {
			if findings {
				body, found = inside, true
			}
			fence = ""
			continue
		}
		inside = append(inside, line)
	}
	if fence != "" && findings {
		return nil, api.Errorf(api.CodeInvalidArgs, "the report's ```%s block at line %d is never closed", findingsInfo, opened).WithSuggestion(reportExample)
	}
	if !found {
		return nil, api.Errorf(api.CodeInvalidArgs, "the report has no ```%s block, so it says nothing of its findings", findingsInfo).WithSuggestion(reportExample)
	}
	var payload pb.AddFindingsRequest
	if err := protojson.Unmarshal([]byte(strings.Join(body, "\n")), &payload); err != nil {
		return nil, api.Errorf(api.CodeInvalidArgs, "the report's ```%s block is not {\"findings\": [...]}: %v", findingsInfo, err).WithSuggestion(reportExample)
	}
	if payload.ReviewId != 0 || payload.SessionId != "" || payload.Report != "" {
		return nil, api.Errorf(api.CodeInvalidArgs, "the report's ```%s block holds findings alone", findingsInfo).WithSuggestion(reportExample)
	}
	return payload.Findings, nil
}

// findingsOf is what req reports: its findings, or its report's, with the
// report kept when it gave one.
func findingsOf(req *pb.AddFindingsRequest) ([]*pb.ReportedFinding, error) {
	if req.Report == "" {
		return req.Findings, nil
	}
	if len(req.Findings) > 0 {
		return nil, api.Errorf(api.CodeInvalidArgs, "give findings or a report that ends with them, not both")
	}
	return reportedFindings(req.Report)
}
