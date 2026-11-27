package worker

import (
	"errors"
	"strings"
	"testing"

	"github.com/semistrict/hotbed/internal/baseimage"
	"github.com/semistrict/hotbed/internal/gitops"
)

// applyOutcome is what Sync acts on in an Apply's result.
type applyOutcome struct {
	Files                                         int
	Err                                           string
	ApplyError, Mutated, RolledBack, Inconsistent bool
}

func outcome(files int, err error) applyOutcome {
	o := applyOutcome{Files: files}
	if err != nil {
		o.Err = err.Error()
	}
	var ae *gitops.ApplyError
	if errors.As(err, &ae) {
		o.ApplyError, o.Mutated, o.RolledBack, o.Inconsistent = true, ae.Mutated, ae.RolledBack, ae.Inconsistent()
	}
	return o
}

// An apply in a rootfs reports what gitops.Apply returned, an ApplyError's
// fields included: Sync treats a mutated checkout as failed and an
// unmutated one as untouched.
func TestApplyInARootfsReportsWhatApplyReturned(t *testing.T) {
	for name, c := range map[string]struct {
		n   int
		err error
	}{
		"applied":                              {3, nil},
		"refused before touching the checkout": {0, errors.New("invalid commit")},
		"rolled back":                          {0, &gitops.ApplyError{Err: errors.New("checkout failed"), Mutated: true, RolledBack: true}},
		"left inconsistent":                    {0, &gitops.ApplyError{Err: errors.New("checkout failed"), Mutated: true, RollbackErr: errors.New("reset failed")}},
	} {
		got, want := outcome(appliedOf(c.n, c.err).result()), outcome(c.n, c.err)
		if got != want {
			t.Errorf("%s: %+v, want %+v", name, got, want)
		}
	}
}

// What an apply in a rootfs says is read only so far, and to its end: the
// apply is never left blocked writing.
func TestReadAtMost(t *testing.T) {
	r := strings.NewReader(strings.Repeat("a", maxSaid*3))
	if got := readAtMost(r, maxSaid); len(got) != maxSaid || r.Len() != 0 {
		t.Errorf("kept %d bytes, left %d unread; want %d kept, none left", len(got), r.Len(), maxSaid)
	}
}

// A sync's bundle is owned on disk by what applies it.
func TestBundleOwner(t *testing.T) {
	spec := baseimage.Spec{User: "dev", UID: 1001, GID: 1002}
	type owner struct {
		uid, gid int
		ok       bool
	}
	for name, c := range map[string]struct {
		e    *Engine
		euid int
		want owner
	}{
		"a rootfs's user":          {&Engine{cfg: EngineConfig{User: "dev", BaseSpec: spec}}, 0, owner{1001, 1002, true}},
		"a rootfs's root":          {&Engine{cfg: EngineConfig{BaseSpec: spec}}, 0, owner{0, 0, false}},
		"a worker that isn't root": {&Engine{cfg: EngineConfig{BaseSpec: spec}}, 501, owner{0, 0, false}},
	} {
		uid, gid, ok := c.e.bundleOwnerAs(c.euid)
		if got := (owner{uid, gid, ok}); got != c.want {
			t.Errorf("%s: %+v, want %+v", name, got, c.want)
		}
	}
}
