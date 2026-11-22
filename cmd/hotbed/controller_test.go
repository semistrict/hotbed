package main

import (
	"testing"

	"github.com/semistrict/hotbed/internal/api"
)

func TestControllerIsNoFlag(t *testing.T) {
	d := fakeDeps()
	d.controller = at("ubu1")
	code, out := hotbed(t, d, "nodes", "--controller", "ubu1")
	if code != exitInvalidArgs || out.Error.Code != api.CodeInvalidArgs {
		t.Errorf("nodes --controller: exit %d, error %+v; want exit 2, INVALID_ARGS", code, out.Error)
	}
}

func TestClientWithoutAControllerSaysHowToGetOne(t *testing.T) {
	code, out := hotbed(t, fakeDeps(), "nodes")
	if code != exitError || out.Command != "nodes" || out.Error.Code != api.CodeNotInitialized {
		t.Errorf("nodes: exit %d, envelope %+v; want exit 1, NOT_INITIALIZED for nodes", code, out)
	}
}

func TestHotbedControllerStandsInOnAMachineThatIsNoWorker(t *testing.T) {
	t.Setenv("HOTBED_CONTROLLER", "ubu1:7121")
	addr, err := localController()
	if err != nil || addr != "ubu1:7121" {
		t.Errorf("localController = %q, %v; want ubu1:7121", addr, err)
	}
}
