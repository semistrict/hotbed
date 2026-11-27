package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
)

// fakeT3 writes a T3 Code CLI whose `thread show THREAD --json` prints
// link as THREAD's link, or, for the thread "down-thread", fails as T3 Code
// does with no server running; it returns its path.
func fakeT3(t *testing.T, link string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "t3")
	src := `#!/bin/sh
[ "$1" = thread ] && [ "$2" = show ] || exit 2
if [ "$3" = down-thread ]; then
  echo "No running T3 Code server uses /tmp/x. Start T3 Code, then retry." >&2
  exit 1
fi
printf '{"thread":{"threadId":"%s","link":"` + link + `","status":"running","activeRunId":null,"pendingRequestCount":0}}\n' "$3"
`
	if err := os.WriteFile(bin, []byte(src), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// A session started in T3 Code keeps the link T3 Code gives its thread, for
// the cockpit to open the thread by.
func TestSessionStartKeepsT3CodeThreadLink(t *testing.T) {
	t.Parallel()
	c := newControllerOnly(t)
	const link = "https://app.t3.codes/threads/env-1/thread-linked"
	c.setenv("T3CODE_CLI_PATH", fakeT3(t, link))
	c.setenv("T3CODE_THREAD_ID", "thread-linked")

	var sess pb.Session
	c.ok(&sess, "session", "start", "linked-otter")
	if sess.T3CodeThreadId != "thread-linked" || sess.T3CodeThreadUrl != link {
		t.Fatalf("session start = thread %q link %q, want thread-linked %s", sess.T3CodeThreadId, sess.T3CodeThreadUrl, link)
	}
	var status sessionStatusResult
	c.ok(&status, "session", "status")
	if status.T3CodeThreadURL != link {
		t.Errorf("session status link = %q, want %s", status.T3CodeThreadURL, link)
	}
	var ls pb.ListSessionsResponse
	c.ok(&ls, "session", "ls")
	if len(ls.Sessions) != 1 || ls.Sessions[0].T3CodeThreadUrl != link {
		t.Errorf("session ls = %v, want linked-otter with %s", ls.Sessions, link)
	}
}

// A session that started without its thread's link takes it when resumed
// with one, and never a link to another thread.
func TestSessionResumeFillsT3CodeThreadLink(t *testing.T) {
	t.Parallel()
	c := newControllerOnly(t)
	c.setenv("T3CODE_THREAD_ID", "thread-later")
	var sess pb.Session
	c.ok(&sess, "session", "start", "later-otter")
	if sess.T3CodeThreadUrl != "" {
		t.Fatalf("session start outside T3 Code's CLI = link %q, want none", sess.T3CodeThreadUrl)
	}

	c.setenv("T3CODE_CLI_PATH", fakeT3(t, "https://app.t3.codes/threads/env-1/thread-other"))
	c.ok(&sess, "session", "start", "later-otter", "--resume", "--t3code-thread-id", "thread-other")
	if sess.T3CodeThreadId != "thread-later" || sess.T3CodeThreadUrl != "" {
		t.Fatalf("resume from another thread = thread %q link %q, want thread-later with no link", sess.T3CodeThreadId, sess.T3CodeThreadUrl)
	}

	c.setenv("T3CODE_CLI_PATH", fakeT3(t, "https://app.t3.codes/threads/env-1/thread-later"))
	c.ok(&sess, "session", "start", "later-otter", "--resume")
	if sess.T3CodeThreadUrl != "https://app.t3.codes/threads/env-1/thread-later" {
		t.Fatalf("resume from its thread = link %q, want its thread's", sess.T3CodeThreadUrl)
	}

	// T3 Code's link for the thread changed (another environment): the
	// session takes the latest.
	c.setenv("T3CODE_CLI_PATH", fakeT3(t, "https://app.t3.codes/threads/env-2/thread-later"))
	c.ok(&sess, "session", "start", "later-otter", "--resume")
	if sess.T3CodeThreadUrl != "https://app.t3.codes/threads/env-2/thread-later" {
		t.Fatalf("resume after T3 Code's link changed = link %q, want the new one", sess.T3CodeThreadUrl)
	}
}

// A session starts without a link T3 Code cannot give, and says so.
func TestSessionStartWarnsWithoutT3CodeThreadLink(t *testing.T) {
	t.Parallel()
	c := newControllerOnly(t)
	c.setenv("T3CODE_CLI_PATH", fakeT3(t, "https://app.t3.codes/threads/env-1/down-thread"))
	c.setenv("T3CODE_THREAD_ID", "down-thread")

	code, stdout, stderr := c.hotbed("--json", "session", "start", "unlinked-otter")
	var out output
	if err := json.Unmarshal([]byte(stdout), &out); err != nil || code != 0 || !out.OK {
		t.Fatalf("session start with T3 Code down: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	var sess pb.Session
	if err := protojson.Unmarshal(out.Data, &sess); err != nil {
		t.Fatal(err)
	}
	if sess.T3CodeThreadId != "down-thread" || sess.T3CodeThreadUrl != "" {
		t.Errorf("session = thread %q link %q, want down-thread with no link", sess.T3CodeThreadId, sess.T3CodeThreadUrl)
	}
	if want := "hotbed: the cockpit will not link to T3 Code thread down-thread: t3 thread show down-thread: exit status 1: No running T3 Code server"; !strings.Contains(stderr, want) {
		t.Errorf("stderr = %q, want it to contain %q", stderr, want)
	}
}

// The cockpit puts the link in an href, so only an https link reaches it: the
// CLI starts the session without any other link T3 Code gives, and says so,
// and the controller refuses one sent anyway.
func TestSessionStartTakesOnlyHTTPSThreadLinks(t *testing.T) {
	t.Parallel()
	c := newControllerOnly(t)
	c.setenv("T3CODE_CLI_PATH", fakeT3(t, "javascript:alert(1)"))
	c.setenv("T3CODE_THREAD_ID", "thread-evil")

	code, stdout, stderr := c.hotbed("--json", "session", "start", "evil-otter")
	var out output
	if err := json.Unmarshal([]byte(stdout), &out); err != nil || code != 0 || !out.OK {
		t.Fatalf("session start with a javascript: link: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	var sess pb.Session
	if err := protojson.Unmarshal(out.Data, &sess); err != nil {
		t.Fatal(err)
	}
	if sess.T3CodeThreadId != "thread-evil" || sess.T3CodeThreadUrl != "" {
		t.Errorf("session = thread %q link %q, want thread-evil with no link", sess.T3CodeThreadId, sess.T3CodeThreadUrl)
	}
	if want := `hotbed: the cockpit will not link to T3 Code thread thread-evil: its link "javascript:alert(1)" is not an https URL`; !strings.Contains(stderr, want) {
		t.Errorf("stderr = %q, want it to contain %q", stderr, want)
	}

	addr, err := c.cli.controller()
	if err != nil {
		t.Fatal(err)
	}
	conn, err := api.Dial(addr, api.DefaultPort, c.cli.http)
	if err != nil {
		t.Fatal(err)
	}
	sessions := hotbedv1connect.NewSessionServiceClient(conn)
	for _, link := range []string{"javascript:alert(1)", "http://app.t3.codes/threads/env-1/thread-evil", "t3code://threads/env-1/thread-evil"} {
		_, err := sessions.StartSession(t.Context(), &pb.StartSessionRequest{
			Id: "evil-otter", Resume: true, T3CodeThreadId: "thread-evil", T3CodeThreadUrl: link,
		})
		if e := api.FromConnect(err); e == nil || e.Code != api.CodeInvalidArgs || e.Message != fmt.Sprintf("t3code_thread_url %q is not an https URL", link) {
			t.Errorf("StartSession with link %s = %v, want INVALID_ARGS: not an https URL", link, err)
		}
	}
}
