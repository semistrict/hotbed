package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/apicmd"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
	"github.com/semistrict/hotbed/internal/gitops"
)

// Reviews of an exact version by reviewer agents: the review commands are
// hotbed.proto's (ReviewService); these are the hooks they name, what they
// need of this checkout and this machine.

// PinReviewedVersion makes review request's base and head, refs as given,
// the exact version to review in this checkout (versionToReview).
func (h cliHooks) PinReviewedVersion(ctx context.Context, _ *apicmd.Call, req *pb.RequestReviewRequest) error {
	v, err := h.versionToReview(ctx, req.Base, req.Head, req.Whole)
	if err != nil {
		return err
	}
	req.Repo, req.Base, req.Head, req.Branch, req.SnapshotOf = v.remote, v.base, v.head, v.branch, v.snapshotOf
	return nil
}

// Commit is the commit that fixed a finding, in full (fixingCommit).
func (h cliHooks) Commit(ctx context.Context, _ *apicmd.Call, text string) (string, error) {
	return h.fixingCommit(ctx, text)
}

// Where is a place a fix changed: PATH, PATH:LINE or PATH:LINE-END.
func (cliHooks) Where(_ context.Context, _ *apicmd.Call, text string) (*pb.Location, error) {
	return parseWhere(text)
}

// ResponsesCursor is where this machine last read a review's responses up
// to: 0, for every response, with --all or none read yet.
func (h cliHooks) ResponsesCursor(_ context.Context, _ *apicmd.Call, req *pb.ListResponsesRequest) (int64, error) {
	if req.All {
		return 0, nil
	}
	return h.responsesCursor(req.ReviewId).read()
}

// SaveResponsesCursor records how far the responses read go, once in hand,
// so the next read goes on from there. Failing to keep it fails nothing:
// the responses are read, and the next read repeats some.
func (h cliHooks) SaveResponsesCursor(_ context.Context, _ *apicmd.Call, req *pb.ListResponsesRequest, res *pb.ListResponsesResponse) error {
	cur := h.responsesCursor(req.ReviewId)
	read, err := cur.read()
	if err == nil && res.Cursor > read {
		err = cur.write(res.Cursor)
	}
	if err != nil {
		fmt.Fprintf(h.stderr, "hotbed: %v\n", err)
	}
	return nil
}

func (h cliHooks) responsesCursor(review int32) cursorFile {
	return cursorFile{
		path:       filepath.Join(h.cache, "reviews", strconv.Itoa(int(review))+".cursor"),
		label:      "responses cursor",
		suggestion: "delete the file to read every response again",
	}
}

// reviewedVersion is what a review is of, in this checkout.
type reviewedVersion struct {
	remote, branch, base, head, snapshotOf string
}

// versionToReview finds the exact version to review in this checkout: head
// (given, or HEAD, or a snapshot commit of uncommitted changes on HEAD),
// pinned under refs/hotbed/reviews so git keeps it, and the base its
// changes are from.
func (h here) versionToReview(ctx context.Context, base, head string, whole bool) (reviewedVersion, error) {
	var v reviewedVersion
	root, err := h.checkoutRoot(ctx)
	if err != nil {
		return v, api.Errorf(api.CodeInvalidArgs, "a review is of a version in a git checkout; this is not in one").
			WithSuggestion("run hotbed review request in the checkout to review")
	}
	g := gitops.Git{Dir: root}
	commit := func(ref string) (string, error) {
		c, err := g.Run(ctx, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
		if err != nil || c == "" {
			return "", api.Errorf(api.CodeInvalidArgs, "%s is not a commit here", ref)
		}
		return c, nil
	}
	if remotes := gitRemoteURLs(ctx, root); len(remotes) > 0 {
		v.remote = remotes[0]
	}
	v.branch, _ = g.Run(ctx, "branch", "--show-current") // empty when detached
	var snap gitops.Snap
	switch {
	case head != "":
		if v.head, err = commit(head); err != nil {
			return v, err
		}
	default:
		if v.head, err = commit("HEAD"); err != nil {
			return v, err
		}
		if snap, err = gitops.Snapshot(ctx, root); err != nil {
			return v, err
		}
		if tree, err := g.Run(ctx, "rev-parse", "HEAD^{tree}"); err != nil {
			return v, err
		} else if snap.Tree != tree {
			v.snapshotOf, v.head = v.head, snap.Commit
		}
	}
	switch {
	case whole && base != "":
		return v, api.Errorf(api.CodeInvalidArgs, "--whole reviews the tree, not changes from a base: give one or the other")
	case whole:
	case base != "":
		if v.base, err = commit(base); err != nil {
			return v, err
		}
	default:
		upstream, err := commit("refs/remotes/origin/HEAD")
		if err != nil {
			return v, api.Errorf(api.CodeInvalidArgs, "no origin/HEAD to find where this work started").
				WithSuggestion("pass --base REF, or --whole; git remote set-head origin --auto records origin/HEAD")
		}
		if v.base, err = g.Run(ctx, "merge-base", v.head, upstream); err != nil {
			return v, err
		}
	}
	if v.base == v.head {
		return v, api.Errorf(api.CodeInvalidArgs, "head %s is base: there are no changes to review", v.head[:8]).
			WithSuggestion("pass --base REF, or --whole for the tree as it is")
	}
	// A snapshot is a commit no branch holds: pin it so gc keeps it while
	// a reviewer may look.
	if _, err := g.Run(ctx, "update-ref", "refs/hotbed/reviews/"+v.head, v.head); err != nil {
		return v, err
	}
	if v.snapshotOf != "" {
		if err := snap.PinSubs(ctx, "refs/hotbed/reviews/"); err != nil {
			return v, err
		}
	}
	return v, nil
}

// fixingCommit is ref in full: resolved in this checkout, or, outside one,
// as given.
func (h here) fixingCommit(ctx context.Context, ref string) (string, error) {
	root, err := h.checkoutRoot(ctx)
	if err != nil {
		return ref, nil // the controller holds it to a full commit
	}
	c, err := gitops.Git{Dir: root}.Run(ctx, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil || c == "" {
		return "", api.Errorf(api.CodeInvalidArgs, "%s is not a commit here", ref).WithSuggestion("pass --commit REF: the commit that fixed them")
	}
	return c, nil
}

// parseWhere is PATH, PATH:LINE or PATH:LINE-END.
func parseWhere(w string) (*pb.Location, error) {
	bad := api.Errorf(api.CodeInvalidArgs, "--where %q: give PATH, PATH:LINE or PATH:LINE-END", w)
	path, lines, hasLines := strings.Cut(w, ":")
	if path == "" {
		return nil, bad
	}
	l := &pb.Location{Path: path}
	if !hasLines {
		return l, nil
	}
	start, end, hasEnd := strings.Cut(lines, "-")
	n, err := strconv.ParseInt(start, 10, 32)
	if err != nil || n < 1 {
		return nil, bad
	}
	l.StartLine = int32(n)
	if hasEnd {
		m, err := strconv.ParseInt(end, 10, 32)
		if err != nil || m < n {
			return nil, bad
		}
		l.EndLine = int32(m)
	}
	return l, nil
}
