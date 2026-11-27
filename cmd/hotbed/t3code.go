package main

import (
	"context"
	"fmt"
	"net/url"

	"github.com/semistrict/hotbed/internal/t3bridge"
)

// t3codeThreadURL is the link T3 Code gives threadID, for the cockpit to
// open the thread by, read with the T3 Code CLI that T3 Code names in its
// agents' environment (T3CODE_CLI_PATH); "" outside T3 Code. The link is
// T3 Code's to make (its host and path are its own), so hotbed never builds
// one. A session needs no link, so one T3 Code cannot give is a warning,
// not a failure.
func (d deps) t3codeThreadURL(ctx context.Context, threadID string) string {
	bin := d.env("T3CODE_CLI_PATH")
	if threadID == "" || bin == "" {
		return ""
	}
	st, err := (&t3bridge.CLIClient{Binary: bin}).ThreadStatus(ctx, threadID)
	switch {
	case err != nil:
		fmt.Fprintf(d.stderr, "hotbed: the cockpit will not link to T3 Code thread %s: %v\n", threadID, err)
	case !st.Exists:
		fmt.Fprintf(d.stderr, "hotbed: the cockpit will not link to T3 Code thread %s: the T3 Code server here (%s) has no such thread\n", threadID, bin)
	case st.Link == "":
		fmt.Fprintf(d.stderr, "hotbed: the cockpit will not link to T3 Code thread %s: %s thread show gave no link\n", threadID, bin)
	case !isHTTPS(st.Link):
		// The controller takes only an https link (the cockpit puts it in an
		// href); sending another would fail the session's start.
		fmt.Fprintf(d.stderr, "hotbed: the cockpit will not link to T3 Code thread %s: its link %q is not an https URL\n", threadID, st.Link)
		return ""
	}
	return st.Link
}

func isHTTPS(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Host != ""
}
