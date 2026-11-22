package main

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"

	"github.com/semistrict/hotbed/internal/api"
)

type uiCmd struct {
	controllerClient
	NoOpen bool `name:"no-open" help:"Print the cockpit URL without opening a browser."`
}

type uiResult struct {
	URL    string `json:"url"`
	Opened bool   `json:"opened"`
}

func (c *uiCmd) Exec(ctx context.Context, d deps) (any, error) {
	url := d.cockpit.home()
	if c.NoOpen {
		return uiResult{URL: url, Opened: false}, nil
	}
	opener := d.openURL
	if opener == nil {
		opener = openDefaultBrowser
	}
	if err := opener(ctx, url); err != nil {
		return nil, fmt.Errorf("open browser for %s: %w", url, err)
	}
	return uiResult{URL: url, Opened: true}, nil
}

func openDefaultBrowser(ctx context.Context, url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", url)
	case "linux":
		cmd = exec.CommandContext(ctx, "xdg-open", url)
	case "windows":
		cmd = exec.CommandContext(ctx, "cmd", "/c", "start", "", url)
	default:
		return api.Errorf(api.CodePlatformNotSupported, "opening a browser is not supported on %s", runtime.GOOS).
			WithSuggestion("open " + url + " manually")
	}
	return cmd.Start()
}
