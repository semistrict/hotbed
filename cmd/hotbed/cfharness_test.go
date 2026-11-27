package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/semistrict/hotbed/internal/cfbundle"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/update"
)

// cfHarness runs a single local Miniflare 5 (workerd) instance of cf-controller
// shared across the test binary, giving each cluster its own isolated SQLite
// Durable Object via the X-Hotbed-Test-DO header.
type cfHarness struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	url   string
	token string
}

var (
	harnessMu   sync.Mutex
	harnessInst *cfHarness
	harnessErr  error
	harnessSeq  atomic.Int64
)

func getCFHarness() (*cfHarness, error) {
	harnessMu.Lock()
	defer harnessMu.Unlock()
	if harnessInst != nil || harnessErr != nil {
		return harnessInst, harnessErr
	}
	harnessInst, harnessErr = startCFHarness()
	return harnessInst, harnessErr
}

func stopCFHarness() {
	harnessMu.Lock()
	h := harnessInst
	harnessInst = nil
	harnessMu.Unlock()
	if h == nil {
		return
	}
	_ = h.stdin.Close()
	done := make(chan struct{})
	go func() {
		_ = h.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = h.cmd.Process.Kill()
		<-done
	}
}

func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "cf-controller", "scripts", "harness.mjs")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("could not locate cf-controller/scripts/harness.mjs from working directory")
		}
		dir = parent
	}
}

func startCFHarness() (*cfHarness, error) {
	root, err := findRepoRoot()
	if err != nil {
		return nil, err
	}
	bundleDir, err := cfbundle.Ensure(root)
	if err != nil {
		return nil, err
	}

	script := filepath.Join(root, "cf-controller", "scripts", "harness.mjs")
	cmd := exec.Command("node", script, "--bundle", bundleDir)
	cmd.Dir = filepath.Join(root, "cf-controller")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start cf-controller harness: %w", err)
	}

	lineCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		if sc.Scan() {
			lineCh <- sc.Text()
			// Drain any further stdout so the child never blocks on a full pipe.
			for sc.Scan() {
			}
			return
		}
		if err := sc.Err(); err != nil {
			errCh <- err
		} else {
			errCh <- io.EOF
		}
	}()

	select {
	case line := <-lineCh:
		var ready struct {
			URL   string `json:"url"`
			Token string `json:"token"`
		}
		if err := json.Unmarshal([]byte(line), &ready); err != nil || ready.URL == "" {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return nil, fmt.Errorf("parse cf-controller harness ready line %q: %w (stderr: %s)", line, err, stderr.String())
		}
		return &cfHarness{cmd: cmd, stdin: stdin, url: ready.URL, token: ready.Token}, nil
	case err := <-errCh:
		_ = cmd.Wait()
		return nil, fmt.Errorf("cf-controller harness exited early (%w): %s", err, stderr.String())
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("timed out waiting for cf-controller harness: %s", stderr.String())
	}
}

func (h *cfHarness) doJSON(ctx context.Context, doName, method, path string, reqBody any, out any) error {
	var bodyReader io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.url+path, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("X-Hotbed-Test-DO", doName)
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode %s %s: %w (body: %s)", method, path, err, raw)
		}
	}
	return nil
}

// testServerConfig replaces controller.ServerConfig in clusterOpts.server.
type testServerConfig struct {
	WakeWait             time.Duration
	AllowLocalRepos      *bool
	Builds               *buildCache
	Build                int64
	UpdatesRepo          string
	UpdatesAPIBase       string
	UpdatesDownloadBase  string
	UpdatesToken         string
	ControllerBuild      int64
	ControllerCommit     string
	ControllerSha256Sums string
}

type mockBuildPayload struct {
	Number int64                       `json:"number"`
	Assets map[string]mockAssetPayload `json:"assets"`
}

type mockAssetPayload struct {
	SHA256        string `json:"sha256"`
	ContentBase64 string `json:"contentBase64"`
}

func buildMockPayload(cache *buildCache, buildNum int64) (*mockBuildPayload, error) {
	assets := make(map[string]mockAssetPayload, len(update.Platforms))
	for _, p := range update.Platforms {
		path := filepath.Join(cache.Dir, fmt.Sprintf("%d", buildNum), p.Asset())
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read cached build %d asset %s: %w", buildNum, p.Asset(), err)
		}
		sum := sha256.Sum256(b)
		assets[p.Asset()] = mockAssetPayload{
			SHA256:        hex.EncodeToString(sum[:]),
			ContentBase64: base64.StdEncoding.EncodeToString(b),
		}
	}
	return &mockBuildPayload{Number: buildNum, Assets: assets}, nil
}

// testServer drives controller maintenance hooks on the test DO.
type testServer struct {
	h      *cfHarness
	doName string
}

func (s *testServer) ReapIdle(ctx context.Context) error {
	return s.h.doJSON(ctx, s.doName, http.MethodPost, "/__test/reap-idle", map[string]any{}, nil)
}

func (s *testServer) TickSchedules(ctx context.Context) error {
	return s.h.doJSON(ctx, s.doName, http.MethodPost, "/__test/tick-schedules", map[string]any{}, nil)
}

func (s *testServer) TickBoard(ctx context.Context) error {
	return s.h.doJSON(ctx, s.doName, http.MethodPost, "/__test/tick-board", map[string]any{}, nil)
}

// testStore inspects and seeds state in the test DO via /__test/* endpoints.
type testStore struct {
	h      *cfHarness
	doName string
}

var unmarshalProto = protojson.UnmarshalOptions{DiscardUnknown: true}

func unmarshalRawProto(raw json.RawMessage, m proto.Message) error {
	return unmarshalProto.Unmarshal(raw, m)
}

func (s *testStore) ConnectedWorkers(ctx context.Context) ([]string, error) {
	var res struct {
		NodeIDs []string `json:"nodeIds"`
	}
	if err := s.h.doJSON(ctx, s.doName, http.MethodGet, "/__test/inspect?kind=connectedWorkers", nil, &res); err != nil {
		return nil, err
	}
	return res.NodeIDs, nil
}

func (s *testStore) LogDaemon(ctx context.Context, level, msg string, attrs map[string]any, at time.Time) error {
	payload := map[string]any{
		"level": level,
		"msg":   msg,
	}
	if len(attrs) > 0 {
		payload["attrs"] = attrs
	}
	if !at.IsZero() {
		payload["at"] = at.UnixMilli()
	}
	return s.h.doJSON(ctx, s.doName, http.MethodPost, "/__test/daemon-log", payload, nil)
}

func (s *testStore) AddProject(ctx context.Context, name string, remotes []string, stamp *pb.Stamp) (*pb.Project, error) {
	proj := map[string]any{
		"name":    name,
		"remotes": remotes,
	}
	if stamp != nil {
		if stamp.At > 0 {
			proj["at"] = stamp.At
		}
		if stamp.Reason != "" {
			proj["reason"] = stamp.Reason
		}
	}
	if err := s.h.doJSON(ctx, s.doName, http.MethodPost, "/__test/seed", map[string]any{"project": proj}, nil); err != nil {
		return nil, err
	}
	return &pb.Project{Name: name, Repos: remotes}, nil
}

func (s *testStore) StartSession(ctx context.Context, id, agent, project, t3codeThreadID string, at time.Time) (*pb.Session, error) {
	sess := map[string]any{
		"id":             id,
		"agent":          agent,
		"project":        project,
		"t3codeThreadId": t3codeThreadID,
	}
	if !at.IsZero() {
		sess["at"] = at.UnixMilli()
	}
	if err := s.h.doJSON(ctx, s.doName, http.MethodPost, "/__test/seed", map[string]any{"session": sess}, nil); err != nil {
		return nil, err
	}
	return &pb.Session{Id: id, Agent: agent, Project: project}, nil
}

func (s *testStore) AddWorkspace(ctx context.Context, id, sessionID, nodeID, agent string, at time.Time) error {
	ws := map[string]any{
		"id":        id,
		"sessionId": sessionID,
		"nodeId":    nodeID,
		"agent":     agent,
	}
	if !at.IsZero() {
		ws["at"] = at.UnixMilli()
	}
	return s.h.doJSON(ctx, s.doName, http.MethodPost, "/__test/seed", map[string]any{"workspace": ws}, nil)
}

func (s *testStore) AddRun(ctx context.Context, run *pb.ScheduledRun) error {
	r := map[string]any{
		"schedule":  run.Schedule,
		"sessionId": run.SessionId,
		"startedAt": run.StartedAt,
		"outcome":   int32(run.Outcome),
	}
	return s.h.doJSON(ctx, s.doName, http.MethodPost, "/__test/seed", map[string]any{"run": r}, nil)
}

func (s *testStore) Session(ctx context.Context, id string) (*pb.Session, error) {
	var res struct {
		Found bool            `json:"found"`
		Data  json.RawMessage `json:"data"`
	}
	if err := s.h.doJSON(ctx, s.doName, http.MethodGet, "/__test/inspect?kind=session&id="+url.QueryEscape(id), nil, &res); err != nil {
		return nil, err
	}
	if !res.Found {
		return nil, fmt.Errorf("session %s not found", id)
	}
	out := &pb.Session{}
	if err := unmarshalRawProto(res.Data, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *testStore) SessionEvents(ctx context.Context, session string, after int64) ([]*pb.SessionEvent, error) {
	var res struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := s.h.doJSON(ctx, s.doName, http.MethodGet, "/__test/inspect?kind=sessionEvents&id="+url.QueryEscape(session), nil, &res); err != nil {
		return nil, err
	}
	out := make([]*pb.SessionEvent, 0, len(res.Data))
	for _, raw := range res.Data {
		ev := &pb.SessionEvent{}
		if err := unmarshalRawProto(raw, ev); err != nil {
			return nil, err
		}
		if ev.Seq > after {
			out = append(out, ev)
		}
	}
	return out, nil
}

func (s *testStore) NodeView(ctx context.Context, nodeID string) (*pb.Checkpoint, error) {
	var res struct {
		Seq        int64             `json:"seq"`
		Bases      []json.RawMessage `json:"bases"`
		Workspaces []json.RawMessage `json:"workspaces"`
		Commands   []json.RawMessage `json:"commands"`
	}
	if err := s.h.doJSON(ctx, s.doName, http.MethodGet, "/__test/inspect?kind=nodeView&id="+url.QueryEscape(nodeID), nil, &res); err != nil {
		return nil, err
	}
	cp := &pb.Checkpoint{Seq: res.Seq}
	for _, raw := range res.Bases {
		b := &pb.Base{}
		if err := unmarshalRawProto(raw, b); err != nil {
			return nil, err
		}
		cp.Bases = append(cp.Bases, b)
	}
	for _, raw := range res.Workspaces {
		w := &pb.Workspace{}
		if err := unmarshalRawProto(raw, w); err != nil {
			return nil, err
		}
		cp.Workspaces = append(cp.Workspaces, w)
	}
	for _, raw := range res.Commands {
		c := &pb.Command{}
		if err := unmarshalRawProto(raw, c); err != nil {
			return nil, err
		}
		cp.Commands = append(cp.Commands, c)
	}
	return cp, nil
}

func (s *testStore) Finished(ctx context.Context, id string) (*pb.FinishedCommand, bool, error) {
	var res struct {
		Found bool            `json:"found"`
		Data  json.RawMessage `json:"data"`
	}
	if err := s.h.doJSON(ctx, s.doName, http.MethodGet, "/__test/inspect?kind=finished&id="+url.QueryEscape(id), nil, &res); err != nil {
		return nil, false, err
	}
	if !res.Found {
		return nil, false, nil
	}
	out := &pb.FinishedCommand{}
	if err := unmarshalRawProto(res.Data, out); err != nil {
		return nil, false, err
	}
	return out, true, nil
}

func (s *testStore) History(ctx context.Context, since time.Time) ([]*pb.FinishedCommand, error) {
	var res struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := s.h.doJSON(ctx, s.doName, http.MethodGet, "/__test/inspect?kind=history", nil, &res); err != nil {
		return nil, err
	}
	sinceMs := int64(0)
	if !since.IsZero() {
		sinceMs = since.UnixMilli()
	}
	out := make([]*pb.FinishedCommand, 0, len(res.Data))
	for _, raw := range res.Data {
		f := &pb.FinishedCommand{}
		if err := unmarshalRawProto(raw, f); err != nil {
			return nil, err
		}
		if sinceMs == 0 || f.EndedAt >= sinceMs {
			out = append(out, f)
		}
	}
	return out, nil
}

func (s *testStore) CommandProfile(ctx context.Context, id string) (*pb.CommandProfile, bool, error) {
	var res struct {
		Found bool            `json:"found"`
		Data  json.RawMessage `json:"data"`
	}
	if err := s.h.doJSON(ctx, s.doName, http.MethodGet, "/__test/inspect?kind=commandProfile&id="+url.QueryEscape(id), nil, &res); err != nil {
		return nil, false, err
	}
	if !res.Found {
		return nil, false, nil
	}
	out := &pb.CommandProfile{}
	if err := unmarshalRawProto(res.Data, out); err != nil {
		return nil, false, err
	}
	return out, true, nil
}

func (s *testStore) BaseByCommand(ctx context.Context, cmdID string) (string, *pb.Base, bool, error) {
	var res struct {
		Found  bool            `json:"found"`
		NodeID string          `json:"nodeId"`
		Base   json.RawMessage `json:"base"`
	}
	if err := s.h.doJSON(ctx, s.doName, http.MethodGet, "/__test/inspect?kind=baseByCommand&id="+url.QueryEscape(cmdID), nil, &res); err != nil {
		return "", nil, false, err
	}
	if !res.Found {
		return "", nil, false, nil
	}
	b := &pb.Base{}
	if err := unmarshalRawProto(res.Base, b); err != nil {
		return "", nil, false, err
	}
	return res.NodeID, b, true, nil
}

func (s *testStore) Command(ctx context.Context, id string) (string, string, error) {
	var res struct {
		Found   bool   `json:"found"`
		Session string `json:"session"`
		Node    string `json:"node"`
	}
	if err := s.h.doJSON(ctx, s.doName, http.MethodGet, "/__test/inspect?kind=command&id="+url.QueryEscape(id), nil, &res); err != nil {
		return "", "", err
	}
	if !res.Found {
		return "", "", fmt.Errorf("command %s not found", id)
	}
	return res.Session, res.Node, nil
}
