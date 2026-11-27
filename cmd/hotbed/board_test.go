package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestBoardCLIEndToEnd exercises all 32 BoardService RPCs through the real
// `hotbed board` CLI against the cf-controller Durable Object test harness
// (satisfies cmd/hotbed/coverage_test.go).
func TestBoardCLIEndToEnd(t *testing.T) {
	t.Parallel()
	c := newControllerOnly(t)

	// Seed two projects so project-scoped board operations work cleanly.
	if _, err := c.store.AddProject(t.Context(), "hotbed", []string{"github.com/org/hotbed"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.store.AddProject(t.Context(), "cladefs", []string{"github.com/org/cladefs"}, nil); err != nil {
		t.Fatal(err)
	}

	cliJSON := func(args ...string) map[string]any {
		t.Helper()
		full := append([]string{"--json"}, args...)
		code, out, stderr := c.hotbed(full...)
		if code != exitOK {
			t.Fatalf("hotbed %s exited %d:\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), code, out, stderr)
		}
		var env map[string]any
		if err := json.Unmarshal([]byte(out), &env); err != nil {
			t.Fatalf("decode JSON for hotbed %s: %v\nraw:\n%s", strings.Join(args, " "), err, out)
		}
		data, _ := env["data"].(map[string]any)
		return data
	}

	cliFail := func(args ...string) string {
		t.Helper()
		full := append([]string{"--json"}, args...)
		code, out, _ := c.hotbed(full...)
		if code == exitOK {
			t.Fatalf("expected hotbed %s to fail, but got exit 0:\n%s", strings.Join(args, " "), out)
		}
		return out
	}

	// 1. Upload & Download attachment
	tmpFile := filepath.Join(t.TempDir(), "hello.txt")
	if err := os.WriteFile(tmpFile, []byte("hello board attachment"), 0o644); err != nil {
		t.Fatal(err)
	}
	up := cliJSON("board", "upload", tmpFile)
	attID, _ := up["id"].(string)
	if attID == "" {
		t.Fatalf("upload returned no attachment id: %v", up)
	}
	dlPath := filepath.Join(t.TempDir(), "downloaded.txt")
	_ = cliJSON("board", "download", attID, "-o", dlPath)
	gotBytes, err := os.ReadFile(dlPath)
	if err != nil || string(gotBytes) != "hello board attachment" {
		t.Fatalf("downloaded bytes = %q, err=%v", string(gotBytes), err)
	}

	// 2. Schema ls, show, create
	schemas := cliJSON("board", "schema", "ls")
	if list, _ := schemas["schemas"].([]any); len(list) < 2 {
		t.Fatalf("expected built-in schemas, got %v", schemas)
	}
	gcSchema := cliJSON("board", "schema", "show", "git-commit@1")
	if gcSchema["id"] != "git-commit@1" {
		t.Fatalf("unexpected git-commit@1 schema: %v", gcSchema)
	}
	createdSchema := cliJSON("board", "schema", "create", "metric@1", "-d", "Test metric",
		"--schema", `{"type":"object","required":["ms"],"properties":{"ms":{"type":"number","minimum":0}}}`)
	if createdSchema["created"] != true {
		t.Fatalf("expected schema created=true: %v", createdSchema)
	}

	// 3. Post, Reply (with --after precondition), Edit (with --if-revision), React, Label (CAS), Delete
	postText := "Initial post\n\n```json#metric@1\n{\"ms\":42}\n```"
	posted := cliJSON("board", "post", "-p", "hotbed", "-l", "status=open", "--attach", attID, "--idempotency-key", "post-1", postText)
	msgObj, _ := posted["message"].(map[string]any)
	msgID, _ := msgObj["id"].(string)
	if msgID == "" {
		t.Fatalf("post returned no message id: %v", posted)
	}
	if url, _ := msgObj["cockpitUrl"].(string); !strings.HasSuffix(url, "/board/"+msgID) {
		t.Fatalf("expected cockpitUrl ending in /board/%s, got %q", msgID, url)
	}

	reply := cliJSON("board", "reply", msgID, "--after", msgID, "First reply!")
	replyMsg, _ := reply["message"].(map[string]any)
	replyID, _ := replyMsg["id"].(string)
	if replyID == "" {
		t.Fatalf("reply returned no id: %v", reply)
	}
	// Stale --after fails
	cliFail("board", "reply", msgID, "--after", msgID, "Stale reply")

	// Edit with revision check
	revStr, _ := msgObj["revision"].(string)
	cliFail("board", "edit", msgID, "--if-revision", "9999", "wrong rev")
	_ = cliJSON("board", "edit", msgID, "--if-revision", revStr, "Edited post")

	// React (add & remove)
	reacted := cliJSON("board", "react", msgID, "thumbsup")
	if rs, _ := reacted["reactions"].([]any); len(rs) != 1 {
		t.Fatalf("expected 1 reaction, got %v", reacted)
	}
	_ = cliJSON("board", "react", msgID, "👍", "--remove")

	// Conditional label CAS
	cliFail("board", "label", msgID, "--if", "status=closed", "status=done")
	labeled := cliJSON("board", "label", msgID, "--if", "status=open", "status=done", "area=cli")
	if lbls, _ := labeled["labels"].(map[string]any); lbls["status"] != "done" {
		t.Fatalf("expected status=done, got %v", labeled)
	}

	// Read list + Read thread + Actors + Wait + Tail
	listed := cliJSON("board", "read", "-p", "hotbed")
	if threads, _ := listed["threads"].([]any); len(threads) != 1 {
		t.Fatalf("expected 1 thread, got %v", listed)
	}
	thread := cliJSON("board", "read", msgID)
	if reps, _ := thread["replies"].([]any); len(reps) != 1 {
		t.Fatalf("expected 1 reply, got %v", thread)
	}
	actors := cliJSON("board", "actors", "-p", "hotbed")
	if as, _ := actors["actors"].([]any); len(as) == 0 {
		t.Fatalf("expected actors, got %v", actors)
	}
	evRes := cliJSON("board", "wait", "-p", "hotbed", "--last", "5", "--timeout", "1s")
	if evs, _ := evRes["events"].([]any); len(evs) == 0 {
		t.Fatalf("expected events from wait --last, got %v", evRes)
	}
	// Tail with --last 1 --count 1 (raw command)
	tailCode, tailOut, tailErr := c.hotbed("--json", "board", "tail", "-p", "hotbed", "--last", "1", "--count", "1")
	if tailCode != exitOK || !strings.Contains(tailOut, `"cursor"`) {
		t.Fatalf("board tail failed: code=%d out=%s err=%s", tailCode, tailOut, tailErr)
	}

	// Delete reply
	deleted := cliJSON("board", "delete", replyID)
	if deleted["deleted"] != true {
		t.Fatalf("expected deleted=true: %v", deleted)
	}

	// 4. Schedules: add, ls, tick, cancel
	sched1 := cliJSON("board", "schedule", "add", "-p", "hotbed", "--in", "60s", "Scheduled ping")
	schedObj1, _ := sched1["schedule"].(map[string]any)
	schedID1, _ := schedObj1["id"].(string)
	if schedID1 == "" {
		t.Fatalf("schedule add returned no id: %v", sched1)
	}
	sched2 := cliJSON("board", "schedule", "add", "-p", "hotbed", "--in", "300s", "To cancel")
	schedObj2, _ := sched2["schedule"].(map[string]any)
	schedID2, _ := schedObj2["id"].(string)
	sList := cliJSON("board", "schedule", "ls", "-p", "hotbed")
	if ss, _ := sList["schedules"].([]any); len(ss) != 2 {
		t.Fatalf("expected 2 pending schedules, got %v", sList)
	}
	canceledSched := cliJSON("board", "schedule", "cancel", schedID2)
	if canceledSched["canceled"] != true {
		t.Fatalf("expected schedule canceled=true: %v", canceledSched)
	}
	// Advance clock and fire sched1
	c.clock.advance(65 * time.Second)
	if err := c.server.TickBoard(t.Context()); err != nil {
		t.Fatal(err)
	}

	// 5. Tasks: create, batch plan, ls, claim, renew, release, complete, fail, reopen, block, unblock, assign, needs, cancel, run
	planFile := filepath.Join(t.TempDir(), "plan.json")
	planJSON := `[
		{"key":"step1","text":"Step 1: prepare"},
		{"key":"step2","text":"Step 2: finish","needs":["step1"]}
	]`
	if err := os.WriteFile(planFile, []byte(planJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	batch := cliJSON("board", "task", "create", "-p", "cladefs", "--plan", planFile)
	batchTasks, _ := batch["tasks"].([]any)
	if len(batchTasks) != 2 {
		t.Fatalf("expected 2 batch tasks, got %v", batch)
	}
	t1ID := batchTasks[0].(map[string]any)["id"].(string)
	t2ID := batchTasks[1].(map[string]any)["id"].(string)

	// Single task create
	single := cliJSON("board", "task", "create", "-p", "cladefs", "Standalone task")
	t3ID := single["task"].(map[string]any)["id"].(string)

	// Task ls
	tList := cliJSON("board", "task", "ls", "-p", "cladefs")
	if ts, _ := tList["tasks"].([]any); len(ts) != 3 {
		t.Fatalf("expected 3 open tasks, got %v", tList)
	}

	// A claim belongs to the claiming actor (principal + profile): it renews, releases,
	// re-claims and completes t1 by acting as itself, with nothing stored locally, while
	// another profile of the same principal can do none of it. Completing t1 readies t2.
	claimed := cliJSON("board", "task", "claim", t1ID, "--lease", "2m", "--as", "worker-a")
	if got := claimed["claim"]; !reflect.DeepEqual(got, map[string]any{"epoch": float64(1), "until": got.(map[string]any)["until"]}) {
		t.Fatalf("claim = %v, want epoch 1 and a lease", got)
	}
	_ = cliJSON("board", "task", "renew", t1ID, "--as", "worker-a")
	_ = cliJSON("board", "task", "release", t1ID, "--as", "worker-a")
	_ = cliJSON("board", "task", "claim", t1ID, "--as", "worker-a")
	reclaimed := cliJSON("board", "task", "claim", t1ID, "--as", "worker-a", "--lease", "5m")
	if got := reclaimed["claim"].(map[string]any)["epoch"]; got != float64(2) {
		t.Fatalf("re-claim by the holder: epoch = %v, want 2 (the same claim, re-leased)", got)
	}
	for _, args := range [][]string{
		{"claim", t1ID},
		{"complete", t1ID, "not mine"},
		{"renew", t1ID},
		{"release", t1ID},
		{"fail", t1ID, "not mine"},
	} {
		out := cliFail(append(append([]string{"board", "task"}, args...), "--as", "worker-b")...)
		if !strings.Contains(out, "worker-a") {
			t.Fatalf("board task %s as worker-b: want a refusal naming holder worker-a, got:\n%s", args[0], out)
		}
	}
	done1 := cliJSON("board", "task", "complete", t1ID, "prepared cleanly", "--as", "worker-a")
	if done1["task"].(map[string]any)["state"] != "BOARD_TASK_STATE_COMPLETED" {
		t.Fatalf("expected t1 completed: %v", done1)
	}

	// Run t2 via `board task run` subprocess (claims ready t2, injects HOTBED_BOARD_TASK)
	runCode, runOut, runErr := c.hotbed("board", "task", "run", t2ID, "--", "sh", "-c", "test -n \"$HOTBED_BOARD_TASK\" && echo task-ran-ok")
	if runCode != exitOK || !strings.Contains(runOut, "task-ran-ok") {
		t.Fatalf("board task run failed: code=%d out=%s err=%s", runCode, runOut, runErr)
	}
	// Fail t2 (still held by this actor after run), reopen, block, unblock, assign, needs, cancel
	_ = cliJSON("board", "task", "fail", t2ID, "flaky test")
	_ = cliJSON("board", "task", "reopen", t2ID)
	_ = cliJSON("board", "task", "block", t2ID, "--kind", "review", "needs sign-off")
	_ = cliJSON("board", "task", "unblock", t2ID)
	_ = cliJSON("board", "task", "assign", t2ID, "--unassign")
	_ = cliJSON("board", "task", "needs", t2ID, t3ID)
	_ = cliJSON("board", "task", "cancel", t2ID, "superseded")

	// 6. Import from mock @vercel/board HTTP server (dry-run + real import + idempotent re-import)
	mockBoard := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/channels":
			_, _ = w.Write([]byte(`{"channels":[{"path":"hotbed","messages":1},{"path":"tasks","messages":2}]}`))
		case "/actors":
			_, _ = w.Write([]byte(`{"actors":[{"id":"tailscale:user:ramon@echophase.com","kind":"person","handle":"ramon","name":"Ramon"}]}`))
		case "/schemas":
			_, _ = w.Write([]byte(`{"schemas":[]}`))
		case "/messages":
			ch := r.URL.Query().Get("channel")
			after := r.URL.Query().Get("after")
			if after != "" && after != "0" {
				_, _ = w.Write([]byte(`{"messages":[]}`))
				return
			}
			if ch == "hotbed" {
				_, _ = w.Write([]byte(`{"messages":[{"id":"14100","channel":"hotbed","text":"Old board note","by":{"actor":"tailscale:user:ramon@echophase.com","principal":"tailscale:user:ramon@echophase.com"},"createdAt":"2026-04-01T12:00:00Z","attachments":[{"id":"att1","name":"log.txt","contentType":"text/plain","size":5}]}]}`))
			} else {
				_, _ = w.Write([]byte(`{"messages":[
					{"id":"14200","channel":"tasks","text":"Old task 1","by":{"actor":"tailscale:user:ramon@echophase.com","principal":"tailscale:user:ramon@echophase.com"},"createdAt":"2026-04-02T12:00:00Z","task":{"state":"completed","owner":"tailscale:user:ramon@echophase.com","by":"tailscale:user:ramon@echophase.com","needs":[]}},
					{"id":"14201","channel":"tasks","text":"Old task 2","by":{"actor":"tailscale:user:ramon@echophase.com","principal":"tailscale:user:ramon@echophase.com"},"createdAt":"2026-04-02T13:00:00Z","task":{"state":"ready","owner":"tailscale:user:ramon@echophase.com","by":"tailscale:user:ramon@echophase.com","needs":["14200"]}},
					{"id":"14202","channel":"tasks","text":"Hotbed task in tasks channel","labels":{"tag.hotbed":"yes"},"by":{"actor":"tailscale:user:ramon@echophase.com","principal":"tailscale:user:ramon@echophase.com"},"createdAt":"2026-04-02T14:00:00Z","task":{"state":"ready","owner":"tailscale:user:ramon@echophase.com","by":"tailscale:user:ramon@echophase.com","needs":[]}},
					{"id":"14203","channel":"tasks","re":"14202","text":"Reply inheriting hotbed project","by":{"actor":"tailscale:user:ramon@echophase.com","principal":"tailscale:user:ramon@echophase.com"},"createdAt":"2026-04-02T14:05:00Z"}
				]}`))
			}
		case "/attachments/att1":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("hello"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockBoard.Close()

	dry := cliJSON("board", "import", "--url", mockBoard.URL, "--map", "tasks=cladefs", "--map-label", "tag.hotbed=yes:hotbed", "--dry-run")
	if dry["messagesFetched"] != float64(5) || dry["dryRun"] != true {
		t.Fatalf("unexpected dry-run summary: %v", dry)
	}
	imp := cliJSON("board", "import", "--url", mockBoard.URL, "--map", "tasks=cladefs", "--map-label", "tag.hotbed=yes:hotbed")
	if imp["messagesImported"] != float64(5) || imp["tasksImported"] != float64(3) || imp["attachmentsCopied"] != float64(1) {
		t.Fatalf("unexpected import summary: %v", imp)
	}
	// Verify preserved original ID #14201 is in cladefs, and #14202 + its reply #14203 routed to hotbed!
	importedThread := cliJSON("board", "read", "14201")
	if importedThread["message"].(map[string]any)["project"] != "cladefs" {
		t.Fatalf("expected #14201 in cladefs, got %v", importedThread)
	}
	hotbedRouted := cliJSON("board", "read", "14202")
	if hotbedRouted["message"].(map[string]any)["project"] != "hotbed" {
		t.Fatalf("expected #14202 routed via --map-label to hotbed, got %v", hotbedRouted)
	}
	if reps, _ := hotbedRouted["replies"].([]any); len(reps) != 1 || reps[0].(map[string]any)["project"] != "hotbed" {
		t.Fatalf("expected reply #14203 to inherit hotbed project, got %v", hotbedRouted)
	}
	// Re-import is idempotent
	imp2 := cliJSON("board", "import", "--url", mockBoard.URL, "--map", "tasks=cladefs", "--map-label", "tag.hotbed=yes:hotbed")
	if imp2["messagesSkipped"] != float64(5) {
		t.Fatalf("expected 5 skipped on re-import, got %v", imp2)
	}

	// Plain-text formatting smoke check (ensures no panics in formatters)
	var plainBuf bytes.Buffer
	plainCode, plainOut, plainErr := c.hotbed("board", "read", "-p", "hotbed")
	plainBuf.WriteString(plainOut)
	if plainCode != exitOK || !strings.Contains(plainBuf.String(), fmt.Sprintf("#%s", msgID)) {
		t.Fatalf("plain board read failed: code=%d out=%s err=%s", plainCode, plainBuf.String(), plainErr)
	}
}
