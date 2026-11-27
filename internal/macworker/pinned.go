package macworker

// Pinned is the machined hotbed's daemon runs a Mac's worker on: a cladefs
// release's (its release workflow's machined-macos job), pinned by SHA-256.
var Pinned = Release{
	Tag:      "cladefs-7abcd75e",
	Machined: Asset{Name: "machined-darwin-arm64", SHA256: "4cca38f814bef4c9b29e8b85a4c21742c3080ee5fe0edd91ad520ff41b4ba460"},
	Helper:   Asset{Name: "machined-vm-darwin-arm64", SHA256: "477f51cf8e99ec9160972e1e85a84621ff7ced5d504df476eb36facf5eb3c2db"},
}
