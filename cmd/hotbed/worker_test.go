package main

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/semistrict/hotbed/internal/api"
)

func TestWorkerInitHelpFlags(t *testing.T) {
	var stdout bytes.Buffer
	d := fakeDeps()
	d.stdout, d.stderr = &stdout, io.Discard
	code := run(t.Context(), []string{"worker", "init", "--help"}, d)
	if code != exitOK {
		t.Fatalf("worker init --help exited %d", code)
	}
	out := stdout.String()
	for _, want := range []string{"--cpus=INT", "--disk-size=", "--memory=", "--root-size=", "--swap-size=", "--tailscale-auth-key="} {
		if !strings.Contains(out, want) {
			t.Errorf("worker init --help missing %q:\n%s", want, out)
		}
	}
}

func TestWorkerInitRequiresController(t *testing.T) {
	t.Setenv("HOTBED_CONTROLLER", "")
	code, out := hotbed(t, fakeDeps(), "worker", "init", "--dir", t.TempDir())
	if code != exitError || out.OK || out.Error == nil || out.Error.Code != api.CodeInvalidArgs {
		t.Fatalf("expected INVALID_ARGS without controller, got exit %d, envelope %+v", code, out)
	}
}

func TestWorkerInitInvalidSize(t *testing.T) {
	code, out := hotbed(t, fakeDeps(), "worker", "init", "ubu1", "--dir", t.TempDir(), "--disk-size", "not-a-size")
	if code != exitError || out.OK || out.Error == nil || out.Error.Code != api.CodeInvalidArgs {
		t.Fatalf("expected INVALID_ARGS for bad --disk-size, got exit %d, envelope %+v", code, out)
	}
}

func TestWorkerStatusUninitialized(t *testing.T) {
	code, out := hotbed(t, fakeDeps(), "worker", "status", "--dir", t.TempDir())
	if code != exitError || out.OK || out.Error == nil || out.Error.Code != api.CodeNotInitialized {
		t.Fatalf("expected NOT_INITIALIZED for empty dir, got exit %d, envelope %+v", code, out)
	}
}
