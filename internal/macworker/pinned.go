package macworker

// Pinned is the machined hotbed's daemon runs a Mac's worker on: a cladefs
// release's (its release workflow's machined-macos job), pinned by SHA-256.
var Pinned = Release{
	Tag:      "cladefs-1b63f179",
	Machined: Asset{Name: "machined-darwin-arm64", SHA256: "b2223a48f8f44961966bad08c5eb3b63ceaf785cc6f748a93adc1cd9d8a9f526"},
	Helper:   Asset{Name: "machined-vm-darwin-arm64", SHA256: "858fcedd617cf93349d4eda5d2b916c25ff7a459f7a27b13555a650b6551d4d3"},
}
