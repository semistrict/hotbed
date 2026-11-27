package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/alecthomas/kong"
)

// here is where a command runs: its environment and its working directory.
// Commands read them from here, never from the process (os.Getenv,
// os.Getwd, a relative path), so that each of a test's CLIs, run side by
// side in one process, can be somewhere of its own.
type here struct {
	getenv func(string) string
	getwd  func() (string, error)
}

// processHere is where the process runs: what main gives its command.
func processHere() here { return here{getenv: os.Getenv, getwd: os.Getwd} }

// env is the variable k, trimmed.
func (h here) env(k string) string { return strings.TrimSpace(h.getenv(k)) }

// t3codeThreadID is the T3 Code thread this runs in, "" for none.
func (h here) t3codeThreadID() string { return h.env("T3CODE_THREAD_ID") }

// checkoutRoot is the top of the git checkout around the working directory;
// it fails outside one.
func (h here) checkoutRoot(ctx context.Context) (string, error) {
	cwd, err := h.getwd()
	if err != nil {
		return "", err
	}
	return gitCheckoutRoot(ctx, cwd)
}

// inCheckout says whether the working directory is in a git checkout.
func (h here) inCheckout(ctx context.Context) (bool, error) {
	cwd, err := h.getwd()
	if err != nil {
		return false, err
	}
	_, err = gitCheckoutRoot(ctx, cwd)
	return err == nil, nil
}

// envFlags is the environment variables kong's env tags name, taken off
// kong, which would read them from the process (os.LookupEnv), to be read
// from here instead once a command line is parsed.
type envFlags map[*kong.Value][]string

// takeEnvs takes every env tag in app off kong and into the envFlags.
func takeEnvs(app *kong.Application) envFlags {
	out := envFlags{}
	_ = kong.Visit(app, func(n kong.Visitable, next kong.Next) error {
		switch v := n.(type) {
		case *kong.Flag:
			v.Envs = nil
		case *kong.Value:
			if len(v.Tag.Envs) > 0 {
				out[v] = v.Tag.Envs
				v.Tag.Envs = nil
			}
		}
		return next(nil)
	})
	return out
}

// helpValue is kong's help for a value, naming the environment variables
// that set it as kong's own would.
func (e envFlags) helpValue(v *kong.Value) string {
	envs, ok := e[v]
	if !ok {
		return kong.DefaultHelpValueFormatter(v)
	}
	tag := *v.Tag
	tag.Envs = envs
	withEnvs := *v
	withEnvs.Tag = &tag
	return kong.DefaultHelpValueFormatter(&withEnvs)
}

// fill sets each value of the parsed command that the command line did not
// give from the first of its environment variables that is not empty.
func (e envFlags) fill(kctx *kong.Context, getenv func(string) string) error {
	given := map[*kong.Value]bool{}
	var values []*kong.Value
	for _, p := range kctx.Path {
		switch {
		case p.Flag != nil:
			given[p.Flag.Value] = true
		case p.Positional != nil:
			given[p.Positional] = true
		}
		var n *kong.Node
		switch {
		case p.App != nil:
			n = p.App.Node
		case p.Command != nil:
			n = p.Command
		}
		if n == nil {
			continue
		}
		for _, f := range n.Flags {
			values = append(values, f.Value)
		}
		values = append(values, n.Positional...)
	}
	for _, v := range values {
		if given[v] {
			continue
		}
		for _, k := range e[v] {
			s := getenv(k)
			if s == "" {
				continue
			}
			if err := v.Parse(kong.ScanFromTokens(kong.Token{Type: kong.FlagValueToken, Value: s}), v.Target); err != nil {
				return fmt.Errorf("%w (from %s=%q)", err, k, s)
			}
			break
		}
	}
	return nil
}
