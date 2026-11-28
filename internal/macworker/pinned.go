package macworker

// Pinned is the machined hotbed's daemon runs a Mac's worker on: a cladefs
// release's (its release workflow's machined-macos job), pinned by SHA-256.
var Pinned = Release{
	Tag:      "cladefs-c9ddcd36",
	Machined: Asset{Name: "machined-darwin-arm64", SHA256: "6136a50c015433442d2d4e4a75c3bab899cfa44d8c577eb87a9f631a6187a5f4"},
	Helper:   Asset{Name: "machined-vm-darwin-arm64", SHA256: "3e224f2fdd5630f125225b5a51fb2c453381ee1e7385795496ffd31dcb5aea72"},
}
