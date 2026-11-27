package main

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
)

func TestStartDevClusterPopulatesCockpit(t *testing.T) {
	dc, err := startDevCluster(t.Context(), "127.0.0.1:0", "")
	if err != nil {
		t.Fatalf("startDevCluster: %v", err)
	}
	t.Cleanup(dc.Close)

	conn, err := api.Dial(dc.addr, api.DefaultPort, http.DefaultClient)
	if err != nil {
		t.Fatalf("dial dev-cockpit: %v", err)
	}
	cluster := hotbedv1connect.NewClusterServiceClient(conn)
	workspaces := hotbedv1connect.NewWorkspaceServiceClient(conn)
	questions := hotbedv1connect.NewQuestionServiceClient(conn)
	reviews := hotbedv1connect.NewReviewServiceClient(conn)

	snap, err := cluster.GetSnapshot(t.Context(), &pb.GetSnapshotRequest{})
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if len(snap.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(snap.Nodes))
	}
	var hosts []string
	for _, n := range snap.Nodes {
		if !n.Online {
			t.Errorf("node %s offline", n.Hostname)
		}
		if n.Profiler == nil || !n.Profiler.Active {
			t.Errorf("node %s profiler not active: %+v", n.Hostname, n.Profiler)
		}
		hosts = append(hosts, n.Hostname)
	}
	slices.Sort(hosts)
	if !slices.Equal(hosts, []string{"asahimbp", "ubu1"}) {
		t.Errorf("node hostnames = %v, want [asahimbp ubu1]", hosts)
	}
	if len(snap.Projects) != 1 || snap.Projects[0].Name != "cladefs" {
		t.Errorf("projects = %v, want [cladefs]", snap.Projects)
	}
	if len(snap.Repos) != 1 || snap.Repos[0].Id != "cladefs" {
		t.Errorf("repos = %v, want [cladefs]", snap.Repos)
	}
	if len(snap.Sessions) != 2 {
		t.Errorf("sessions = %d, want 2", len(snap.Sessions))
	}
	for _, s := range snap.Sessions {
		switch s.Agent {
		case "claude", "codex", "antigravity", "":
			t.Errorf("session %s has disallowed or empty agent label %q", s.Id, s.Agent)
		}
	}
	if len(snap.Workspaces) != 2 {
		t.Errorf("workspaces = %d, want 2", len(snap.Workspaces))
	}
	if len(snap.History) < 2 {
		t.Errorf("history = %d, want at least 2 finished commands", len(snap.History))
	}
	var runningCmds []*pb.Command
	hasServer := false
	for _, c := range snap.Commands {
		if c.State == pb.CommandState_COMMAND_STATE_RUNNING {
			runningCmds = append(runningCmds, c)
			if c.Kind == "server" {
				hasServer = true
			}
			if c.Url != "" {
				t.Errorf("running command %s advertises unreachable URL %q, want empty", c.Id, c.Url)
			}
		}
	}
	if len(runningCmds) != 2 {
		t.Fatalf("running commands = %d, want 2", len(runningCmds))
	}
	if !hasServer {
		t.Errorf("expected a running server command among %+v", runningCmds)
	}
	if snap.OpenQuestions != 2 {
		t.Errorf("openQuestions = %d, want 2", snap.OpenQuestions)
	}
	if len(snap.Schedules) != 1 || snap.Schedules[0].Name != "nightly-nextest" {
		t.Errorf("schedules = %v, want [nightly-nextest]", snap.Schedules)
	}

	// Verify diagnostics and continuous profiling work on seeded running commands and nodes.
	for _, cmd := range runningCmds {
		insp, err := workspaces.InspectCommand(t.Context(), &pb.InspectCommandRequest{Id: cmd.Id})
		if err != nil {
			t.Fatalf("InspectCommand(%s): %v", cmd.Id, err)
		}
		if len(insp.Procs) == 0 || len(insp.Files) == 0 || insp.CgroupStats == nil || len(insp.Kstacks) == 0 {
			t.Errorf("InspectCommand(%s) missing diagnostics: procs=%d files=%d cgroup=%v kstacks=%d",
				cmd.Id, len(insp.Procs), len(insp.Files), insp.CgroupStats != nil, len(insp.Kstacks))
		}
		prof, err := workspaces.GetProfile(t.Context(), &pb.GetProfileRequest{
			Target: &pb.GetProfileRequest_CommandId{CommandId: cmd.Id},
		})
		if err != nil {
			t.Fatalf("GetProfile(%s): %v", cmd.Id, err)
		}
		if !prof.Active || prof.TotalSamples == 0 || len(prof.Stacks) == 0 || len(prof.Frames) == 0 {
			t.Errorf("GetProfile(%s) empty or inactive: active=%v total=%d stacks=%d frames=%d",
				cmd.Id, prof.Active, prof.TotalSamples, len(prof.Stacks), len(prof.Frames))
		}
	}
	nodeProf, err := workspaces.GetProfile(t.Context(), &pb.GetProfileRequest{
		Target: &pb.GetProfileRequest_Node{Node: "ubu1"},
	})
	if err != nil {
		t.Fatalf("GetProfile(ubu1): %v", err)
	}
	if !nodeProf.Active || nodeProf.TotalSamples == 0 || len(nodeProf.Stacks) == 0 {
		t.Errorf("GetProfile(ubu1) empty or inactive: active=%v total=%d stacks=%d",
			nodeProf.Active, nodeProf.TotalSamples, len(nodeProf.Stacks))
	}

	qRes, err := questions.ListQuestions(t.Context(), &pb.ListQuestionsRequest{
		Scope: &pb.ListQuestionsRequest_AllProjects{AllProjects: true},
	})
	if err != nil {
		t.Fatalf("ListQuestions: %v", err)
	}
	if len(qRes.Questions) != 2 {
		t.Errorf("ListQuestions = %d, want 2", len(qRes.Questions))
	}

	rRes, err := reviews.ListReviews(t.Context(), &pb.ListReviewsRequest{
		Scope: &pb.ListReviewsRequest_AllProjects{AllProjects: true},
	})
	if err != nil {
		t.Fatalf("ListReviews: %v", err)
	}
	if len(rRes.Reviews) != 1 {
		t.Fatalf("ListReviews = %d, want 1", len(rRes.Reviews))
	}
	rev, err := reviews.GetReview(t.Context(), &pb.GetReviewRequest{Id: rRes.Reviews[0].Id})
	if err != nil {
		t.Fatalf("GetReview: %v", err)
	}
	if len(rev.Findings) != 2 {
		t.Errorf("review findings = %d, want 2", len(rev.Findings))
	}

	// Verify Vite proxy-style browser requests (Host: localhost:5173, same-origin)
	// pass sameMachineBrowser cleanly.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+dc.addr+"/hotbed.v1.ClusterService/GetSnapshot", bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "localhost:5173"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxied GetSnapshot: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("proxied GetSnapshot status = %d, want 200", resp.StatusCode)
	}
}

// The dev cluster admits whoever reaches it as the owner, and its workers
// run real processes: it refuses what a web page in the owner's browser
// could send it from another site, by DNS rebinding or a cross-site request.
func TestDevClusterRejectsOtherSitesRequests(t *testing.T) {
	dc, err := startDevCluster(t.Context(), "127.0.0.1:0", "")
	if err != nil {
		t.Fatalf("startDevCluster: %v", err)
	}
	t.Cleanup(dc.Close)

	for _, tc := range []struct {
		name, host, site, origin string
		status                   int
	}{
		{"the CLI", "", "", "", http.StatusOK},
		{"the Vite proxy", "localhost:5173", "same-origin", "http://localhost:5173", http.StatusOK},
		{"a top-level navigation", "localhost:7120", "none", "", http.StatusOK},
		{"DNS rebinding", "evil.example:7120", "", "", http.StatusForbidden},
		{"a cross-site fetch", "127.0.0.1:7120", "cross-site", "https://evil.example", http.StatusForbidden},
		{"another site's origin", "127.0.0.1:7120", "", "https://evil.example", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+dc.addr+"/hotbed.v1.ClusterService/ListNodes", strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			if tc.host != "" {
				req.Host = tc.host
			}
			if tc.site != "" {
				req.Header.Set("Sec-Fetch-Site", tc.site)
			}
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("ListNodes: %v", err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Errorf("status = %d, want %d: %s", resp.StatusCode, tc.status, body)
			}
		})
	}
}

// It listens on loopback alone.
func TestDevClusterRefusesANonLoopbackAddress(t *testing.T) {
	_, err := startDevCluster(t.Context(), "0.0.0.0:0", "")
	if err == nil || !strings.Contains(err.Error(), "dev-cockpit listens only on loopback") {
		t.Fatalf("startDevCluster on 0.0.0.0 = %v, want a refusal to listen off loopback", err)
	}
}

// The cockpit is served by the Worker itself, from the built UI directory, to
// every deep link: the same asset path as production, behind the same gate.
func TestDevClusterServesTheBuiltCockpitThroughTheWorker(t *testing.T) {
	ui := t.TempDir()
	if err := os.WriteFile(filepath.Join(ui, "index.html"), []byte("<!doctype html><title>dev cockpit</title>"), 0o644); err != nil {
		t.Fatal(err)
	}
	dc, err := startDevCluster(t.Context(), "127.0.0.1:0", ui)
	if err != nil {
		t.Fatalf("startDevCluster: %v", err)
	}
	t.Cleanup(dc.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+dc.addr+"/sessions/brave-otter", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/html")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET deep link: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "<title>dev cockpit</title>") {
		t.Errorf("GET deep link = %d %q, want 200 with the built index.html", resp.StatusCode, body)
	}
}

func TestInitSampleRepoIgnoresGlobalCommitSigning(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "gitconfig")
	cfg := "[commit]\n\tgpgsign = true\n[tag]\n\tgpgsign = true\n[gpg]\n\tprogram = /nonexistent/gpg\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfgPath)

	dir, sha, err := initSampleRepo(filepath.Join(tmp, "cladefs"))
	if err != nil {
		t.Fatalf("initSampleRepo with global gpgsign failed: %v", err)
	}
	if dir == "" || sha == "" {
		t.Fatalf("initSampleRepo returned dir=%q sha=%q, want non-empty", dir, sha)
	}
}
