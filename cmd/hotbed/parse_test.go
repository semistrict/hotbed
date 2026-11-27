package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// noEnv is an empty environment.
func noEnv(string) string { return "" }

// A flag the command does not know, before "--", is refused: kong would pass
// it, and every flag after it, through as the command, which the args after
// "--" then replace, dropping them unnoticed (an older hotbed given a newer
// flag, such as --keep-idle, ran without it and without the --mem after it).
func TestUnknownFlagBeforeDashDashIsRefused(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"run", "--no-such-flag", "--mem", "8G", "--", "true"},
		{"shell", "--no-such-flag", "--", "true"},
	} {
		_, _, _, _, err := parse(args, io.Discard, noEnv)
		want := "INVALID_ARGS: unknown flag --no-such-flag of hotbed " + args[0] + " (before --)"
		if err == nil || err.Error() != want {
			t.Errorf("parse %q: err %v, want %q", args, err, want)
		}
	}
	if _, _, _, _, err := parse([]string{"run", "make", "--", "-j8"}, io.Discard, noEnv); err == nil || err.Error() != "INVALID_ARGS: unexpected argument make of hotbed run (before --)" {
		t.Errorf("parse run make -- -j8: err %v", err)
	}

	cmd, _, _, _, err := parse([]string{"run", "--keep-idle", "--mem", "8G", "--", "make", "test"}, io.Discard, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	run := cmd.(*runCmd)
	if !run.KeepIdle || run.Mem != "8G" || !slices.Equal(run.Args, []string{"make", "test"}) {
		t.Errorf("run --keep-idle --mem 8G -- make test = keepIdle %v, mem %q, args %q", run.KeepIdle, run.Mem, run.Args)
	}
}

// The variables kong's env tags name are the command's environment, never the
// process's: a command line gives a value over them, an empty one is none,
// and help still names them. (Not parallel: it sets the process's variables,
// to show that they are not read.)
func TestEnvTagsReadTheCommandsEnvironment(t *testing.T) {
	t.Setenv("HOTBED_SESSION", "from-process")
	t.Setenv("HOTBED_CONTROLLER", "https://from-process.example")
	env := newTestHere(t, map[string]string{"HOTBED_SESSION": "from-here", "HOTBED_CONTROLLER": "https://from-here.example"})

	session := func(getenv func(string) string, args ...string) string {
		t.Helper()
		cmd, _, _, _, err := parse(args, io.Discard, getenv)
		if err != nil {
			t.Fatalf("parse %q: %v", args, err)
		}
		switch c := cmd.(type) {
		case *runCmd:
			return c.Session
		case *apiCommand:
			return c.Args.Session
		}
		t.Fatalf("parse %q gave a %T", args, cmd)
		return ""
	}
	for _, tc := range []struct{ args, given []string }{
		{[]string{"run", "--", "true"}, []string{"run", "-s", "given", "--", "true"}},
		{[]string{"question", "ls"}, []string{"question", "ls", "-s", "given"}},
	} {
		if got := session(env.getenv, tc.args...); got != "from-here" {
			t.Errorf("%q in an environment naming from-here: session %q", tc.args, got)
		}
		if got := session(noEnv, tc.args...); got != "" {
			t.Errorf("%q in an empty environment: session %q, want none (not the process's)", tc.args, got)
		}
		if got := session(env.getenv, tc.given...); got != "given" {
			t.Errorf("%q: session %q, want given", tc.given, got)
		}
	}

	// A positional argument's too.
	cmd, _, _, _, err := parse([]string{"login"}, io.Discard, env.getenv)
	if err != nil {
		t.Fatal(err)
	}
	if got := cmd.(*loginCmd).Controller; got != "https://from-here.example" {
		t.Errorf("login's controller = %q, want https://from-here.example", got)
	}

	var help bytes.Buffer
	if _, _, _, code, _ := parse([]string{"run", "--help"}, &help, noEnv); code == nil {
		t.Fatal("run --help did not exit")
	}
	if !strings.Contains(help.String(), "($HOTBED_SESSION)") {
		t.Errorf("run --help does not name $HOTBED_SESSION:\n%s", help.String())
	}
}

// Commands read their environment and working directory from deps.here
// alone: one that read the process's would see another test's, or the
// developer's shell's, when tests run side by side.
func TestCommandsReadNoProcessEnvironment(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") || name == "here.go" {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "os" && slices.Contains([]string{"Getenv", "LookupEnv", "Getwd", "Chdir"}, sel.Sel.Name) {
				t.Errorf("%s: os.%s; read deps.here (here.go) instead", fset.Position(sel.Pos()), sel.Sel.Name)
			}
			return true
		})
	}
}
