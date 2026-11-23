package unit

import (
	"slices"
	"strings"
	"testing"
)

func TestShellStartArgsRequiresRootDir(t *testing.T) {
	if _, _, err := shellStartArgs(Spec{Name: "hotbed-sh-no-root"}, ShellIO{}); err == nil {
		t.Fatal("shellStartArgs accepted empty RootDir, want error")
	}
}

func TestShellStartArgsChrootCustomArgv(t *testing.T) {
	fakeHost(t)
	s := Spec{
		Name:       "hotbed-sh-xyz",
		RootDir:    "/var/lib/hotbed/vol/ws/w1",
		Argv:       []string{"htop", "--tree"},
		Dir:        "/work/app",
		Tmp:        "/var/lib/hotbed/vol/ws/w1/.tmp/sh",
		User:       "ramon",
		PathPrefix: []string{"/work/.hotbed/bin"},
	}
	args, env, err := shellStartArgs(s, ShellIO{PTY: false})
	if err != nil {
		t.Fatal(err)
	}

	if !slices.Contains(args, "--slice="+DefaultSlice) {
		t.Errorf("want default slice, got %v", args)
	}
	if len(args) < 2 || args[len(args)-2] != "htop" || args[len(args)-1] != "--tree" {
		t.Errorf("custom argv missing from chroot shell args: %v", args)
	}
	if !slices.Contains(env, "TERM=dumb") {
		t.Errorf("non-PTY should default TERM=dumb, got %v", env)
	}
	if !slices.Contains(env, "PATH=/work/.hotbed/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin") {
		t.Errorf("PATH env missing: %v", env)
	}
}

func TestShellStartArgsChroot(t *testing.T) {
	fakeHost(t)
	s := Spec{
		Name:          "hotbed-sh-abc123",
		RootDir:       "/var/lib/hotbed/vol/ws/w1",
		Dir:           "/work/app",
		User:          "ramon",
		Binds:         []Bind{{Source: "/mnt/scratch", Target: "/mnt/scratch"}},
		ReadOnlyBinds: []Bind{{Source: "/var/lib/hotbed/git-auth", Target: "/run/hotbed/git-auth"}},
		Env:           []string{"HOME=/work/.hotbed/home"},
	}
	args, env, err := shellStartArgs(s, ShellIO{PTY: true, Term: "xterm-256color"})
	if err != nil {
		t.Fatal(err)
	}
	sep := slices.Index(args, "--")
	if sep < 0 {
		t.Fatalf("missing -- in %q", args)
	}
	want := []string{
		"/usr/bin/unshare", "--mount", "--propagation", "slave", "--",
		"/bin/sh", "-eu", "-c", shellOOMScoreAdj + chrootScript, "hotbed-shell",
		"/var/lib/hotbed/vol/ws/w1", "/work/app", "1000:1000", "1000,27",
		"1", "1",
		"/mnt/scratch", "/mnt/scratch",
		"/var/lib/hotbed/git-auth", "/run/hotbed/git-auth",
		"/bin/bash",
	}
	if got := args[sep+1:]; !slices.Equal(got, want) {
		t.Errorf("command = %q\nwant      %q", got, want)
	}
	if !slices.Contains(env, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin") || !slices.Contains(env, "HOME=/work/.hotbed/home") {
		t.Errorf("env = %q", env)
	}
}

// A shell is the OOM killer's first pick as a command is:
// its script raises its own oom_score_adj before it starts anything.
func TestShellsAreTheOOMKillersFirstPick(t *testing.T) {
	fakeHost(t)
	s := Spec{Name: "hotbed-sh-a", RootDir: "/vol/ws/w"}
	args, _, err := shellStartArgs(s, ShellIO{})
	if err != nil {
		t.Fatal(err)
	}
	i := slices.Index(args, "-c")
	if i < 0 || !strings.HasPrefix(args[i+1], "echo 1000 > /proc/self/oom_score_adj\n") {
		t.Errorf("shell %+v: script does not first set oom_score_adj 1000: %q", s, args)
	}
}

// StartShell runs in a transient systemd scope, which has no ExecContext and
// rejects SELinuxContext= with "Unknown assignment" (board 7921).
func TestShellStartArgsScopeOmitsSELinuxContext(t *testing.T) {
	fakeHost(t)
	origSELinux := hasSELinux
	t.Cleanup(func() { hasSELinux = origSELinux })

	for _, enabled := range []bool{true, false} {
		hasSELinux = func() bool { return enabled }
		args, _, err := shellStartArgs(Spec{Name: "hotbed-sh-se", RootDir: "/vol/ws/w"}, ShellIO{})
		if err != nil {
			t.Fatalf("selinux=%v: %v", enabled, err)
		}
		for _, a := range args {
			if strings.Contains(a, "SELinuxContext") {
				t.Errorf("selinux=%v: shell scope must not pass SELinuxContext (systemd-run --scope rejects it): %q", enabled, args)
			}
		}
	}
}
