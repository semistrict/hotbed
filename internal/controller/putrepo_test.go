package controller

import (
	"testing"
	"time"

	"github.com/semistrict/hotbed/internal/api"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

func TestPutRepoURLValidation(t *testing.T) {
	ctx := t.Context()
	store, err := OpenStore(ctx, t.TempDir()+"/hotbed.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.UnixMilli(1795320000000)
	srv := NewServer(ServerConfig{Store: store, Now: func() time.Time { return now }})

	valid := []struct {
		id  string
		url string
	}{
		{"https-repo", "https://github.com/semistrict/hotbed.git"},
		{"http-repo", "http://127.0.0.1:54321/semistrict/hotbed.git"},
		{"ssh-repo", "ssh://git@github.com/semistrict/hotbed.git"},
		{"scp-repo", "git@github.com:semistrict/hotbed.git"},
		{"scp-userless-github", "github.com:org/repo.git"},
		{"scp-userless-alias", "my-git-host:team/repo.git"},
		{"scp-userless-ipv6", "[2001:db8::1]:org/repo.git"},
	}
	for _, tc := range valid {
		if _, err := srv.PutRepo(ctx, &pb.PutRepoRequest{
			Repo: &pb.Repo{Id: tc.id, Url: tc.url},
		}); err != nil {
			t.Errorf("PutRepo(%s, %q) = %v, want nil", tc.id, tc.url, err)
		}
	}

	invalid := []string{
		"",
		"   ",
		" https://github.com/o/r.git",
		"https://github.com/o/r.git\n",
		"https://github.com/o/r.git\x00",
		"-u",
		"--upload-pack=/tmp/pwn",
		"--upload-pack=touch /tmp/pwn",
		"ext::sh -c id",
		"ext::/bin/sh",
		"EXT::/bin/sh",
		"fd::17/foo",
		"FD::17/foo",
		"remote-ext::/bin/sh",
		"Remote-Ext::/bin/sh",
		"git://github.com/o/r.git",
		"file:///tmp/repo",
		"/tmp/repo",
		"./repo",
		"ssh://-oProxyCommand=id/repo",
		"git@-oProxyCommand=id:o/r",
		"ext::sh",
		"foo::bar",
		"file:/tmp/repo",
		"ext:sh",
		"C:/Users/repo",
		"host:-upload-pack",
		"-host:org/repo",
		"foo/bar:baz",
		"@github.com:org/repo",
		"git@a@b:org/repo",
	}
	for _, u := range invalid {
		_, err := srv.PutRepo(ctx, &pb.PutRepoRequest{
			Repo: &pb.Repo{Id: "bad-repo", Url: u},
		})
		if code := api.CodeOf(err); code != api.CodeInvalidArgs {
			t.Errorf("PutRepo(bad-repo, %q) = %v (code %q), want %q", u, err, code, api.CodeInvalidArgs)
		}
	}

	repos, err := store.Repos(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range repos {
		if r.Id == "bad-repo" {
			t.Fatalf("rejected repo was persisted: %+v", r)
		}
	}
}

func TestPutRepoAllowLocalRepos(t *testing.T) {
	ctx := t.Context()
	store, err := OpenStore(ctx, t.TempDir()+"/hotbed.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.UnixMilli(1795320000000)
	srv := NewServer(ServerConfig{
		Store:           store,
		Now:             func() time.Time { return now },
		AllowLocalRepos: true,
	})

	for _, tc := range []struct {
		id  string
		url string
	}{
		{"abs-local", "/tmp/hotbed-test-origin"},
		{"file-local", "file:///tmp/hotbed-test-origin"},
		{"https-remote", "https://github.com/semistrict/hotbed.git"},
	} {
		if _, err := srv.PutRepo(ctx, &pb.PutRepoRequest{
			Repo: &pb.Repo{Id: tc.id, Url: tc.url},
		}); err != nil {
			t.Errorf("PutRepo with AllowLocalRepos(%s, %q) = %v, want nil", tc.id, tc.url, err)
		}
	}

	stillInvalid := []string{
		"",
		"--upload-pack=/tmp/pwn",
		"-u",
		"ext::/bin/sh",
		"EXT::/bin/sh",
		"fd::17/foo",
		"remote-ext::/bin/sh",
		"./relative",
		"git://github.com/o/r.git",
		"https://github.com/o/r.git\x00",
		"ext::sh",
		"file:/tmp/repo",
		"host:-upload-pack",
		"-host:org/repo",
		"foo/bar:baz",
	}
	for _, u := range stillInvalid {
		_, err := srv.PutRepo(ctx, &pb.PutRepoRequest{
			Repo: &pb.Repo{Id: "still-bad", Url: u},
		})
		if code := api.CodeOf(err); code != api.CodeInvalidArgs {
			t.Errorf("PutRepo with AllowLocalRepos(still-bad, %q) = %v (code %q), want %q", u, err, code, api.CodeInvalidArgs)
		}
	}
}
