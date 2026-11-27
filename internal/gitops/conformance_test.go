package gitops

import (
	"encoding/json"
	"os"
	"testing"
)

// The cases of testdata/conformance/gitops.json, which cf-controller's
// gitops.ts runs too (cf-controller/test/conformance.test.ts): the controller
// must accept exactly the repo URLs the worker does, and name a remote as it
// does.

type gitopsFixture struct {
	Validate struct {
		Valid     []string `json:"valid"`
		Invalid   []string `json:"invalid"`
		LocalOnly []string `json:"localOnly"`
	} `json:"validate"`
	Canonical [][2]string `json:"canonical"`
}

func loadGitopsFixture(t *testing.T) gitopsFixture {
	t.Helper()
	data, err := os.ReadFile("../../testdata/conformance/gitops.json")
	if err != nil {
		t.Fatal(err)
	}
	var f gitopsFixture
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Validate.Valid) == 0 || len(f.Validate.Invalid) == 0 || len(f.Validate.LocalOnly) == 0 || len(f.Canonical) == 0 {
		t.Fatal("gitops.json is missing a group of cases")
	}
	return f
}

func TestValidateRepoURLConformance(t *testing.T) {
	f := loadGitopsFixture(t)
	for _, u := range f.Validate.Valid {
		t.Run("valid "+u, func(t *testing.T) {
			if err := ValidateRepoURL(u); err != nil {
				t.Errorf("ValidateRepoURL(%q) = %v, want accepted", u, err)
			}
			if err := ValidateRepoURLAllowLocal(u); err != nil {
				t.Errorf("ValidateRepoURLAllowLocal(%q) = %v, want accepted", u, err)
			}
		})
	}
	for _, u := range f.Validate.Invalid {
		t.Run("invalid "+u, func(t *testing.T) {
			if err := ValidateRepoURL(u); err == nil {
				t.Errorf("ValidateRepoURL(%q) accepted it", u)
			}
			if err := ValidateRepoURLAllowLocal(u); err == nil {
				t.Errorf("ValidateRepoURLAllowLocal(%q) accepted it", u)
			}
		})
	}
	for _, u := range f.Validate.LocalOnly {
		t.Run("local only "+u, func(t *testing.T) {
			if err := ValidateRepoURL(u); err == nil {
				t.Errorf("ValidateRepoURL(%q) accepted a local repo", u)
			}
			if err := ValidateRepoURLAllowLocal(u); err != nil {
				t.Errorf("ValidateRepoURLAllowLocal(%q) = %v, want accepted", u, err)
			}
		})
	}
}

func TestCanonicalRemoteConformance(t *testing.T) {
	for _, c := range loadGitopsFixture(t).Canonical {
		raw, want := c[0], c[1]
		t.Run(raw, func(t *testing.T) {
			if got := CanonicalRemote(raw); got != want {
				t.Errorf("CanonicalRemote(%q) = %q, want %q", raw, got, want)
			}
		})
	}
}
