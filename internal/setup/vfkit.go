package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// vfkit VM states (GET /vm/state "state"), as the Virtualization framework
// names them.
const (
	vfkitStateRunning  = "VirtualMachineStateRunning"
	vfkitStatePaused   = "VirtualMachineStatePaused"
	vfkitStateStarting = "VirtualMachineStateStarting"
)

// vfkit state changes (POST /vm/state).
const (
	// vfkitStop asks the guest to power off, as its power button would.
	vfkitStop = "Stop"
	// vfkitHardStop pulls the VM's plug.
	vfkitHardStop = "HardStop"
	vfkitResume   = "Resume"
)

// vfkitClient talks to vfkit's REST API on its Unix socket.
func vfkitClient(sock string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
		DisableKeepAlives: true,
	}}
}

// vfkitVMState returns the VM's state from vfkit on sock, such as
// vfkitStateRunning.
func vfkitVMState(ctx context.Context, sock string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://vfkit/vm/state", nil)
	if err != nil {
		return "", err
	}
	body, err := vfkitDo(sock, req)
	if err != nil {
		return "", err
	}
	var st struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(body, &st); err != nil {
		return "", fmt.Errorf("parse vfkit VM state %q: %w", strings.TrimSpace(string(body)), err)
	}
	if st.State == "" {
		return "", fmt.Errorf("vfkit VM state has no state: %s", strings.TrimSpace(string(body)))
	}
	return st.State, nil
}

// vfkitSetVMState asks vfkit on sock to change the VM's state, such as
// vfkitStop.
func vfkitSetVMState(ctx context.Context, sock, state string) error {
	body, err := json.Marshal(map[string]string{"state": state})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://vfkit/vm/state", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	_, err = vfkitDo(sock, req)
	return err
}

func vfkitDo(sock string, req *http.Request) ([]byte, error) {
	resp, err := vfkitClient(sock).Do(req)
	if err != nil {
		return nil, fmt.Errorf("vfkit %s %s: %w", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("vfkit %s %s: read response: %w", req.Method, req.URL.Path, err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("vfkit %s %s: %s: %s", req.Method, req.URL.Path, resp.Status, strings.TrimSpace(string(body)))
	}
	return body, nil
}
