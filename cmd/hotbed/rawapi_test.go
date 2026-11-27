package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveAPIEndpoint(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in       string
		path     string
		rawQuery string
		isRPC    bool
		wantErr  bool
	}{
		{"/healthz", "/healthz", "", false, false},
		{"healthz", "/healthz", "", false, false},
		{"/healthz?foo=1", "/healthz", "foo=1", false, false},
		{"ClusterService/GetSnapshot", "/hotbed.v1.ClusterService/GetSnapshot", "", true, false},
		{"hotbed.v1.ClusterService/ListNodes", "/hotbed.v1.ClusterService/ListNodes", "", true, false},
		{"/hotbed.v1.SessionService/ListSessions", "/hotbed.v1.SessionService/ListSessions", "", true, false},
		{"https://example.com/healthz", "", "", false, true},
	}
	for _, tc := range cases {
		p, q, rpc, err := resolveAPIEndpoint(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("resolveAPIEndpoint(%q) succeeded, want error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("resolveAPIEndpoint(%q) error: %v", tc.in, err)
		}
		if p != tc.path || q != tc.rawQuery || rpc != tc.isRPC {
			t.Fatalf("resolveAPIEndpoint(%q) = (%q, %q, %v), want (%q, %q, %v)", tc.in, p, q, rpc, tc.path, tc.rawQuery, tc.isRPC)
		}
	}
}

func TestRawAPICommand(t *testing.T) {
	t.Parallel()
	var lastMethod, lastPath, lastQuery, lastAuth, lastCT string
	var lastBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastMethod = r.Method
		lastPath = r.URL.Path
		lastQuery = r.URL.RawQuery
		lastAuth = r.Header.Get("Authorization")
		lastCT = r.Header.Get("Content-Type")
		lastBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/not-found" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"nope"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"count":3,"items":["a","b"]}`))
	}))
	defer srv.Close()

	where := newTestHere(t, map[string]string{"HOTBED_TOKEN": "hbo_test_secret"})

	runAPI := func(args ...string) (int, string, string) {
		var out, errBuf bytes.Buffer
		d := deps{
			here:       where.here(),
			http:       srv.Client(),
			stream:     srv.Client(),
			controller: func() (string, error) { return srv.URL, nil },
			stdin:      strings.NewReader(`{"from":"stdin"}`),
			stdout:     &out,
			stderr:     &errBuf,
		}
		code := run(context.Background(), append([]string{"api"}, args...), d)
		return code, out.String(), errBuf.String()
	}

	// 1. Plain GET endpoint with automatic bearer token and --jq.
	code, out, errOut := runAPI("/healthz", "--jq", ".count")
	if code != exitOK {
		t.Fatalf("GET /healthz exit=%d stderr=%q", code, errOut)
	}
	if strings.TrimSpace(out) != "3" {
		t.Fatalf("GET /healthz --jq .count = %q, want 3", out)
	}
	if lastMethod != http.MethodGet || lastPath != "/healthz" || lastAuth != "Bearer hbo_test_secret" {
		t.Fatalf("unexpected GET request: %s %s auth=%q", lastMethod, lastPath, lastAuth)
	}

	// 2. ConnectRPC shorthand defaults to POST + application/json + {}.
	code, out, errOut = runAPI("ClusterService/GetSnapshot")
	if code != exitOK {
		t.Fatalf("ConnectRPC shorthand exit=%d stderr=%q", code, errOut)
	}
	if lastMethod != http.MethodPost || lastPath != "/hotbed.v1.ClusterService/GetSnapshot" || lastCT != "application/json" || string(lastBody) != "{}" {
		t.Fatalf("unexpected RPC request: %s %s ct=%q body=%q", lastMethod, lastPath, lastCT, string(lastBody))
	}
	if !strings.Contains(out, `"ok":true`) {
		t.Fatalf("unexpected RPC response: %q", out)
	}

	// 3. Typed (-F) and raw (-f) fields serialize as JSON body.
	tmpFile := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(tmpFile, []byte("from-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errOut = runAPI("SessionService/ListSessions", "-f", "project=hotbed", "-F", "all=true", "-F", "limit=10", "-F", "note=@"+tmpFile)
	if code != exitOK {
		t.Fatalf("fields exit=%d stderr=%q", code, errOut)
	}
	var gotBody map[string]any
	if err := json.Unmarshal(lastBody, &gotBody); err != nil {
		t.Fatalf("unmarshal sent body %q: %v", string(lastBody), err)
	}
	if gotBody["project"] != "hotbed" || gotBody["all"] != true || gotBody["limit"] != float64(10) || gotBody["note"] != "from-file" {
		t.Fatalf("unexpected JSON fields: %#v", gotBody)
	}

	// 4. GET with -f sends query parameters.
	code, _, errOut = runAPI("/healthz", "-X", "GET", "-f", "foo=bar")
	if code != exitOK {
		t.Fatalf("GET query exit=%d stderr=%q", code, errOut)
	}
	if lastQuery != "foo=bar" {
		t.Fatalf("GET query = %q, want foo=bar", lastQuery)
	}

	// 5. --input - reads stdin, -i prints headers, 404 exits non-zero.
	code, out, errOut = runAPI("/not-found", "-i", "--input", "-")
	if code != exitError {
		t.Fatalf("404 exit=%d, want %d", code, exitError)
	}
	if string(lastBody) != `{"from":"stdin"}` {
		t.Fatalf("stdin body = %q", string(lastBody))
	}
	if !strings.Contains(out, "404 Not Found") || !strings.Contains(out, `{"error":"nope"}`) {
		t.Fatalf("404 -i output = %q", out)
	}
	if !strings.Contains(errOut, "404 Not Found") {
		t.Fatalf("404 stderr = %q", errOut)
	}
}

func TestControllerAuthToken(t *testing.T) {
	dir := t.TempDir()
	tokFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokFile, []byte("  hbo_file_token \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	orig := defaultHotbedDir
	defaultHotbedDir = func() (string, error) { return dir, nil }
	t.Cleanup(func() { defaultHotbedDir = orig })

	where := newTestHere(t, nil)
	if got := controllerAuthToken(where.getenv, "ubu1:7120"); got != "" {
		t.Fatalf("tailnet controller got token %q, want empty", got)
	}
	if got := controllerAuthToken(where.getenv, "http://127.0.0.1:7120"); got != "" {
		t.Fatalf("plaintext http controller got token %q, want empty", got)
	}
	if got := controllerAuthToken(where.getenv, "https://ctrl.example.workers.dev"); got != "hbo_file_token" {
		t.Fatalf("https controller got token %q, want hbo_file_token", got)
	}
	where.setenv("HOTBED_TOKEN", "hbo_env_token")
	if got := controllerAuthToken(where.getenv, "https://ctrl.example.workers.dev"); got != "hbo_env_token" {
		t.Fatalf("env override got token %q, want hbo_env_token", got)
	}
}

// Every plain HTTP request the CLI makes of its controller says who calls
// the same way: the token as a bearer, when there is one, and this
// machine's name; a header the caller set is kept.
func TestIdentify(t *testing.T) {
	t.Parallel()
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	h := http.Header{}
	identify(h, "hbo_tok")
	if h.Get("Authorization") != "Bearer hbo_tok" || h.Get("X-Hotbed-Host") != host {
		t.Errorf("identify = %v, want the bearer token and %s", h, host)
	}
	h = http.Header{}
	identify(h, "")
	if _, ok := h["Authorization"]; ok || h.Get("X-Hotbed-Host") != host {
		t.Errorf("identify without a token = %v, want no Authorization, and %s", h, host)
	}
	h = http.Header{"Authorization": {"Bearer given"}, "X-Hotbed-Host": {"elsewhere"}}
	identify(h, "hbo_tok")
	if h.Get("Authorization") != "Bearer given" || h.Get("X-Hotbed-Host") != "elsewhere" {
		t.Errorf("identify over given headers = %v, want them kept", h)
	}
}
