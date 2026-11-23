package gitops

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateRepoURL(t *testing.T) {
	valid := []string{
		"https://github.com/semistrict/hotbed.git",
		"HTTPS://github.com/semistrict/hotbed",
		"http://127.0.0.1:54321/semistrict/hotbed.git",
		"ssh://git@github.com/semistrict/hotbed.git",
		"ssh://git@127.0.0.1:2222/semistrict/hotbed.git",
		"https://[::1]:8443/org/repo.git",
		"git@github.com:semistrict/hotbed.git",
		"git@github.com:/semistrict/hotbed.git",
		"git@[::1]:semistrict/hotbed.git",
		"git@[2001:db8::1]:org/repo.git",
		"ubuntu@my-host.ts.net:repo.git",
		"user.name+1@gitlab.example.com:group/sub/repo.git",
		"github.com:org/repo",
		"github.com:org/repo.git",
		"github.com:/org/repo.git",
		"my-git-host:team/repo.git",
		"myhost:team/repo.git",
		"[::1]:org/repo.git",
		"[2001:db8::1]:org/repo.git",
		"github.com:org/repo@v2.git",
		"[2001:db8::1]:org/repo@v2.git",
	}
	for _, u := range valid {
		if err := ValidateRepoURL(u); err != nil {
			t.Errorf("ValidateRepoURL(%q) = %v, want nil", u, err)
		}
		if err := ValidateRepoURLAllowLocal(u); err != nil {
			t.Errorf("ValidateRepoURLAllowLocal(%q) = %v, want nil", u, err)
		}
	}

	alwaysInvalid := []string{
		"",
		"   ",
		" https://github.com/o/r.git",
		"https://github.com/o/r.git ",
		"https://github.com/o/r.git\n",
		"https://github.com/o/r.git\t",
		"https://github.com/o/r.git\x00",
		"https://github.com/o/r\x07.git",
		"\xffhttps://github.com/o/r.git",
		"-u",
		"--upload-pack=/tmp/pwn",
		"--upload-pack=touch /tmp/pwn",
		"ext::sh -c id",
		"ext::/bin/sh",
		"EXT::/bin/sh",
		"eXt::/bin/sh",
		"fd::17/foo",
		"FD::17/foo",
		"remote-ext::/bin/sh",
		"Remote-Ext::/bin/sh",
		"REMOTE-EXT::/bin/sh",
		"ext::https://github.com/o/r.git",
		"git::https://github.com/o/r.git",
		"git://github.com/o/r.git",
		"ftp://example.com/o/r.git",
		"./repo",
		"../repo",
		"file:/etc/passwd",
		"ssh://-oProxyCommand=id/repo",
		"ssh://git@-oProxyCommand=id/repo",
		"ssh://-oProxyCommand=id@github.com/o/r",
		"-oProxyCommand=id@github.com:o/r",
		"git@-oProxyCommand=id:o/r",
		"git@github.com::ext",
		"git@github.com:-flag/repo",
		"https://github.com",
		"https://github.com/",
		"https:///org/repo",
		"https://github.com:0/o/r",
		"https://github.com:99999/o/r",
		"git@github.com:",
		"git@github.com:/",
		"github.com:",
		"github.com:/",
		"[::1]:",
		"[::1]:/",
		"[::1]::ext",
		"git@[::1]::ext",
		"[fe80::1%eth0]:org/repo.git",
		"ext::sh",
		"foo::bar",
		"my-helper::/bin/sh",
		"foo::bar@github.com:org/repo",
		"file:/tmp/repo",
		"FILE:/tmp/repo",
		"file:foo",
		"ext:sh",
		"EXT:sh",
		"fd:17/foo",
		"remote-ext:foo",
		"http:github.com/o/r",
		"https:github.com/o/r",
		"git:github.com/o/r",
		"ftp:example.com/r",
		"ssh:github.com/o/r",
		"C:/Users/repo",
		"C:\\Users\\repo",
		"c:org/repo",
		"host:-upload-pack",
		"[::1]:-flag",
		"-host:org/repo",
		"foo/bar:baz",
		"a/b@github.com:org/repo",
		"@github.com:org/repo",
		"git@:org/repo",
		":org/repo",
		"git@a@b:org/repo",
		"just-a-bare-name",
		"/",
		"file:///",
		"file://evil.example/tmp/repo",
	}
	for _, u := range alwaysInvalid {
		if err := ValidateRepoURL(u); err == nil {
			t.Errorf("ValidateRepoURL(%q) = nil, want error", u)
		}
		if err := ValidateRepoURLAllowLocal(u); err == nil {
			t.Errorf("ValidateRepoURLAllowLocal(%q) = nil, want error", u)
		}
	}

	localOnly := []string{
		"file:///tmp/repo",
		"FILE:///tmp/repo",
		"file://localhost/tmp/repo",
		"/tmp/repo",
		"/var/folders/test/origin",
	}
	for _, u := range localOnly {
		if err := ValidateRepoURL(u); err == nil {
			t.Errorf("ValidateRepoURL(%q) = nil, want error", u)
		}
		if err := ValidateRepoURLAllowLocal(u); err != nil {
			t.Errorf("ValidateRepoURLAllowLocal(%q) = %v, want nil", u, err)
		}
	}
}

func TestValidateSHA(t *testing.T) {
	sha1 := strings.Repeat("a1", 20)
	sha1Upper := strings.Repeat("A1", 20)
	sha256 := strings.Repeat("0f", 32)
	for _, s := range []string{sha1, sha1Upper, sha256} {
		if err := ValidateSHA(s); err != nil {
			t.Errorf("ValidateSHA(%q) = %v, want nil", s, err)
		}
	}

	invalid := []string{
		"",
		"HEAD",
		"main",
		"--force",
		"--output=/tmp/pwn",
		"-" + strings.Repeat("a", 39),
		"-" + strings.Repeat("a", 40),
		strings.Repeat("a", 39),
		strings.Repeat("a", 41),
		strings.Repeat("a", 63),
		strings.Repeat("a", 65),
		strings.Repeat("g", 40),
		sha1[:39] + "\n",
		sha1[:39] + "\x00",
		sha1 + "^{commit}",
	}
	for _, s := range invalid {
		if err := ValidateSHA(s); err == nil {
			t.Errorf("ValidateSHA(%q) = nil, want error", s)
		}
	}
}

func installTrapGit(t *testing.T) (marker string) {
	t.Helper()
	binDir := t.TempDir()
	marker = filepath.Join(t.TempDir(), "git-spawned")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + marker + "\"\nexit 1\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	return marker
}

func TestApplyRejectsInvalidInputsBeforeSpawningGit(t *testing.T) {
	validSHA := strings.Repeat("a", 40)
	validBundle := filepath.Join(t.TempDir(), "sync.bundle")
	dir := t.TempDir()

	cases := []struct {
		name   string
		bundle string
		commit string
		base   string
		subs   []SubCheckout
	}{
		{"leading-dash commit", validBundle, "--force", validSHA, nil},
		{"leading-dash base", validBundle, validSHA, "--output=/tmp/pwn", nil},
		{"non-hex commit", validBundle, "HEAD", validSHA, nil},
		{"non-hex base", validBundle, validSHA, "main", nil},
		{"leading-dash bundle", "--upload-pack=/tmp/pwn", validSHA, validSHA, nil},
		{"empty bundle", "", validSHA, validSHA, nil},
		{"sub leading-dash commit", validBundle, validSHA, validSHA, []SubCheckout{{Path: "lib", Commit: "--force", Bundle: validBundle}}},
		{"sub leading-dash bundle", validBundle, validSHA, validSHA, []SubCheckout{{Path: "lib", Commit: validSHA, Bundle: "--upload-pack=/tmp/pwn"}}},
		{"sub escaping path", validBundle, validSHA, validSHA, []SubCheckout{{Path: "../escape", Commit: validSHA, Bundle: validBundle}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			marker := installTrapGit(t)
			if _, err := Apply(t.Context(), Git{Dir: dir}, tc.bundle, tc.commit, tc.base, tc.subs); err == nil {
				t.Fatalf("Apply succeeded, want error")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("git was spawned before rejecting invalid input (marker exists: %v)", err)
			}
		})
	}
}

func TestBundleRejectsInvalidInputsBeforeSpawningGit(t *testing.T) {
	validSHA := strings.Repeat("b", 40)
	dir := t.TempDir()

	cases := []struct {
		name   string
		commit string
		base   string
	}{
		{"leading-dash base", validSHA, "--all"},
		{"leading-dash commit", "--force", validSHA},
		{"non-hex base", validSHA, "HEAD"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			marker := installTrapGit(t)
			if _, err := Bundle(t.Context(), Snap{Root: dir, Commit: tc.commit}, tc.base, &bytes.Buffer{}); err == nil {
				t.Fatalf("Bundle succeeded, want error")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("git was spawned before rejecting invalid input (marker exists: %v)", err)
			}
		})
	}
}

func TestGitRunPassesProtocolExtAllowNever(t *testing.T) {
	t.Run("prepends -c protocol.ext.allow=never to argv", func(t *testing.T) {
		binDir := t.TempDir()
		argsFile := filepath.Join(t.TempDir(), "argv")
		script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + argsFile + "\"\n"
		if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", binDir)

		if _, err := (Git{Dir: t.TempDir()}).Run(t.Context(), "status"); err != nil {
			t.Fatalf("Git.Run: %v", err)
		}
		b, err := os.ReadFile(argsFile)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		if len(lines) < 3 || lines[0] != "-c" || lines[1] != "protocol.ext.allow=never" || lines[2] != "status" {
			t.Fatalf("git argv = %q, want [-c protocol.ext.allow=never status]", lines)
		}
	})

	t.Run("real git blocks ext:: even when repo config allows it", func(t *testing.T) {
		dir := t.TempDir()
		git(t, dir, "init", "--quiet", "-b", "main")
		git(t, dir, "config", "protocol.ext.allow", "always")

		marker := filepath.Join(t.TempDir(), "pwned")
		_, err := (Git{Dir: dir}).Run(t.Context(), "ls-remote", "--", "ext::touch "+marker)
		if err == nil {
			t.Fatalf("ls-remote ext:: succeeded, want error")
		}
		if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
			t.Fatalf("ext:: helper executed despite -c protocol.ext.allow=never")
		}
	})
}
