// Package version says which build of hotbed this is. CI stamps each build
// it publishes with its number (the workflow's run number, which only
// grows), as the release build-N; a binary built by hand is a dev build,
// build 0. The controller runs the newest build and every node follows it.
package version

import (
	"runtime/debug"
	"strconv"
	"strings"
)

// build is set by CI: go build -ldflags "-X
// github.com/semistrict/hotbed/internal/version.build=N".
var build string

// Number is this binary's build, 0 for a dev build.
func Number() int64 { return parse(build) }

// String says this binary's build and commit, as "build 42 (0123456789ab)"
// or "dev (0123456789ab)".
func String() string { return describe(build, commit()) }

// Of is the build a String names, 0 for a dev build or none.
func Of(s string) int64 {
	n, ok := strings.CutPrefix(s, "build ")
	if !ok {
		return 0
	}
	n, _, _ = strings.Cut(n, " ")
	return parse(n)
}

// Tag is build n's release tag.
func Tag(n int64) string { return "build-" + strconv.FormatInt(n, 10) }

// ParseTag is a release tag's build, 0 for a tag that names none.
func ParseTag(tag string) int64 {
	s, ok := strings.CutPrefix(tag, "build-")
	if !ok {
		return 0
	}
	return parse(s)
}

func parse(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 1 {
		return 0
	}
	return n
}

func describe(build, commit string) string {
	s := "dev"
	if n := parse(build); n > 0 {
		s = "build " + strconv.FormatInt(n, 10)
	}
	if commit != "" {
		s += " (" + commit[:min(12, len(commit))] + ")"
	}
	return s
}

func commit() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			return s.Value
		}
	}
	return ""
}
