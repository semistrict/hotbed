package main

import (
	"bytes"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"connectrpc.com/connect/v2"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gen/hotbed/v1/hotbedv1connect"
)

func TestStartDevClusterPopulatesCockpit(t *testing.T) {
	dc, err := startDevCluster(t.Context(), "127.0.0.1:0")
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
	// pass httpx.Gate cleanly.
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

func TestDevClusterRejectsOtherLocalUID(t *testing.T) {
	dc, err := startDevCluster(t.Context(), "127.0.0.1:0")
	if err != nil {
		t.Fatalf("startDevCluster: %v", err)
	}
	t.Cleanup(dc.Close)

	ownerUID := uint32(os.Getuid())
	otherUID := ownerUID + 1
	if otherUID == 0 {
		otherUID = 1
	}

	// Simulate a loopback connection opened by a different local OS user.
	dc.tn.setLocalUID(func(netip.AddrPort, netip.AddrPort) (uint32, error) {
		return otherUID, nil
	})

	client := &http.Client{Transport: &http.Transport{}}
	t.Cleanup(client.CloseIdleConnections)

	// Plain HTTP request must be rejected with 403 Forbidden.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+dc.addr+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("other UID GET / status = %d, want 403", resp.StatusCode)
	}

	// ConnectRPC request must be rejected with PERMISSION_DENIED / NOT_AUTHORIZED.
	conn, err := api.Dial(dc.addr, api.DefaultPort, client)
	if err != nil {
		t.Fatalf("dial dev-cockpit: %v", err)
	}
	cluster := hotbedv1connect.NewClusterServiceClient(conn)
	_, err = cluster.GetSnapshot(t.Context(), &pb.GetSnapshotRequest{})
	if err == nil {
		t.Fatalf("other UID GetSnapshot unexpectedly succeeded")
	}
	if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
		t.Errorf("other UID GetSnapshot connect code = %v, want %v (err: %v)", got, connect.CodePermissionDenied, err)
	}
	if got := api.CodeOf(err); got != api.CodeNotAuthorized {
		t.Errorf("other UID GetSnapshot api code = %q, want %q", got, api.CodeNotAuthorized)
	}

	// Direct external connections to a dev worker listener must also be rejected.
	wreq, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+dc.workers[0].ln.Addr().String()+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	wresp, err := client.Do(wreq)
	if err != nil {
		t.Fatalf("GET worker: %v", err)
	}
	wresp.Body.Close()
	if wresp.StatusCode != http.StatusUnauthorized && wresp.StatusCode != http.StatusForbidden {
		t.Errorf("external GET worker status = %d, want 401 or 403", wresp.StatusCode)
	}

	// Restore os.Getuid() and verify the owner is admitted again.
	dc.tn.setLocalUID(func(netip.AddrPort, netip.AddrPort) (uint32, error) {
		return ownerUID, nil
	})
	if _, err := cluster.GetSnapshot(t.Context(), &pb.GetSnapshotRequest{}); err != nil {
		t.Fatalf("owner UID GetSnapshot failed: %v", err)
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
