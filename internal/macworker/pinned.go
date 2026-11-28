package macworker

// Pinned is the machined hotbed's daemon runs a Mac's worker on: a cladefs
// release's (its release workflow's machined-macos job), pinned by SHA-256.
var Pinned = Release{
	Tag:      "cladefs-c8cf65ec",
	Machined: Asset{Name: "machined-darwin-arm64", SHA256: "e28f5f1d13ea38c6e64b372da4312565277e464a45f93de69845067cd427897b"},
	Helper:   Asset{Name: "machined-vm-darwin-arm64", SHA256: "e727c981a3eed4abe92bbc93d617e2237740040c32af403b82a4767bc3b41ae2"},
}
