// The cases of internal/gitops's validate_test.go, so the controller accepts
// exactly the repo URLs the worker does.
import { describe, expect, it } from "vite-plus/test";
import { validateRepoURL } from "../src/gitops.ts";

const valid = [
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
];

const alwaysInvalid = [
	"",
	"   ",
	" https://github.com/o/r.git",
	"https://github.com/o/r.git ",
	"https://github.com/o/r.git\n",
	"https://github.com/o/r.git\t",
	"https://github.com/o/r.git\x00",
	"https://github.com/o/r\x07.git",
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
	// Bracketed hosts are IP literals, and IPv6 is IPv6.
	"[]:repo",
	"[-flag]:repo",
	"[not-an-ip]:repo",
	"[1.2.3.256]:repo",
	"[01.2.3.4]:repo",
	"[1::2::3]:repo",
	"[:]:repo",
	"[::::]:repo",
	"https://[::::]/org/repo",
	"https://github.com\\org/repo",
	// A file:// URL the URL parser refuses is invalid, not an internal error.
	"file://[/tmp/repo",
];

const localOnly = [
	"file:///tmp/repo",
	"FILE:///tmp/repo",
	"file://localhost/tmp/repo",
	"/tmp/repo",
	"/var/folders/test/origin",
];

describe("validateRepoURL, as internal/gitops", () => {
	it.each(valid)("accepts %j", (u) => {
		expect(() => validateRepoURL(u)).not.toThrow();
		expect(() => validateRepoURL(u, true)).not.toThrow();
	});

	it.each(alwaysInvalid)("refuses %j as invalid arguments", (u) => {
		for (const allowLocal of [false, true]) {
			expect(() => validateRepoURL(u, allowLocal)).toThrow(
				expect.objectContaining({ code: "INVALID_ARGS" }),
			);
		}
	});

	it.each(localOnly)("accepts %j only where local repos are allowed", (u) => {
		expect(() => validateRepoURL(u)).toThrow(expect.objectContaining({ code: "INVALID_ARGS" }));
		expect(() => validateRepoURL(u, true)).not.toThrow();
	});

	it("names what is wrong with a bracketed host", () => {
		expect(() => validateRepoURL("[not-an-ip]:repo")).toThrow(
			'repo URL "[not-an-ip]:repo" has invalid IP literal "not-an-ip"',
		);
		expect(() => validateRepoURL("file://[/tmp/repo", true)).toThrow(
			'invalid file repo URL "file://[/tmp/repo"',
		);
	});
});
