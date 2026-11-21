package api

import "time"

// Repo is a git repository the cluster keeps warm bases of.
type Repo struct {
	ID     string `json:"id"`     // short name, such as ratel
	URL    string `json:"url"`    // what git clones
	Branch string `json:"branch"` // the branch bases follow; empty means the remote's default
	// Freshen brings a checkout up to date the way a person would after a
	// pull: install dependencies, build, build the tests. Bases run it, so
	// every fork starts with its output.
	Freshen []string `json:"freshen"`
	// Hosts are the nodes (host names) that keep bases; empty means every worker.
	Hosts []string `json:"hosts"`
	// MemoryLow and MemoryHigh are the limits a freshen runs with.
	CreatedAt time.Time `json:"createdAt"`
}

type BaseState string

const (
	BaseBuilding BaseState = "building"
	BaseWarm     BaseState = "warm"
	BaseStale    BaseState = "stale"
	BaseFailed   BaseState = "failed"
)

// Base is one generation of a repo's branch, checked out, freshened and
// snapshotted on one node.
type Base struct {
	ID         string    `json:"id"` // <repo>-g<generation>, unique on its node
	RepoID     string    `json:"repoId"`
	NodeID     string    `json:"nodeId"`
	Generation int       `json:"generation"`
	Commit     string    `json:"commit"`
	Message    string    `json:"message"`
	State      BaseState `json:"state"`
	Progress   float64   `json:"progress"` // 0..1 while building: freshen steps done
	BuiltAt    *string   `json:"builtAt"`
	// BuildSeconds is how long this generation took; Cold says it started
	// from nothing, so its time is what a fork saves.
	BuildSeconds float64  `json:"buildSeconds"`
	Cold         bool     `json:"cold"`
	SizeBytes    int64    `json:"sizeBytes"`
	Toolchain    string   `json:"toolchain"`
	Baked        []string `json:"baked"`
	// Units is how many units (cargo crates) a cold freshen compiled.
	Units   int    `json:"units"`
	Command string `json:"command"` // the freshen command's ID, for its log
}

type WorkspaceState string

const (
	WorkspaceActive   WorkspaceState = "active"
	WorkspaceIdle     WorkspaceState = "idle"
	WorkspaceReleased WorkspaceState = "released"
)

// Workspace is a writable fork of a base that one agent works in.
type Workspace struct {
	ID             string         `json:"id"`
	RepoID         string         `json:"repoId"`
	BaseID         string         `json:"baseId"`
	NodeID         string         `json:"nodeId"`
	Agent          string         `json:"agent"`
	Branch         string         `json:"branch"`
	Head           string         `json:"head"` // the commit last synced, or the base's
	BaseCommit     string         `json:"baseCommit"`
	Tree           string         `json:"tree"` // the tree last synced, to skip a sync that changes nothing
	FilesChanged   int            `json:"filesChanged"`
	State          WorkspaceState `json:"state"`
	CreatedAt      string         `json:"createdAt"`
	ForkMillis     int64          `json:"forkMillis"`
	ExclusiveBytes int64          `json:"exclusiveBytes"`
	SharedBytes    int64          `json:"sharedBytes"`
	LastActiveAt   string         `json:"lastActiveAt"`
	// Path is where commands see the checkout, such as /work/ratel.
	Path string `json:"path"`
}

type CommandState string

const (
	CommandQueued    CommandState = "queued"
	CommandRunning   CommandState = "running"
	CommandParked    CommandState = "parked"
	CommandSucceeded CommandState = "succeeded"
	CommandFailed    CommandState = "failed"
	CommandCancelled CommandState = "cancelled"
)

// Live reports whether a command has not ended.
func (s CommandState) Live() bool {
	return s == CommandQueued || s == CommandRunning || s == CommandParked
}

type CommandLimits struct {
	CPUWeight       int   `json:"cpuWeight"`
	MemoryLowBytes  int64 `json:"memoryLowBytes"`
	MemoryHighBytes int64 `json:"memoryHighBytes"`
}

type Rebuilt struct {
	Units int `json:"units"`
	Of    int `json:"of"`
}

type CommandEvent struct {
	At     string `json:"at"`
	What   string `json:"what"` // queued, admitted, parked, thawed, exited, cancelled
	Detail string `json:"detail"`
}

// Command is a command in a workspace, live or just ended.
type Command struct {
	ID          string         `json:"id"`
	WorkspaceID string         `json:"workspaceId"`
	NodeID      string         `json:"nodeId"`
	Argv        string         `json:"argv"` // shell-quoted, for display
	Args        []string       `json:"args"`
	Kind        string         `json:"kind"` // build, test, check, server, shell, freshen
	State       CommandState   `json:"state"`
	QueuedAt    string         `json:"queuedAt"`
	StartedAt   *string        `json:"startedAt"`
	EndedAt     *string        `json:"endedAt"`
	ExitCode    *int           `json:"exitCode"`
	Limits      CommandLimits  `json:"limits"`
	CPUCores    float64        `json:"cpuCores"`
	MemoryBytes int64          `json:"memoryBytes"`
	SwapBytes   int64          `json:"swapBytes"`
	Rebuilt     *Rebuilt       `json:"rebuilt"`
	ColdSeconds *float64       `json:"coldSeconds"`
	Reason      *string        `json:"reason"`
	URL         *string        `json:"url"`
	Timeline    []CommandEvent `json:"timeline"`
	// Attached commands stop when no client has followed their output for
	// a while; detached ones run until stopped.
	Attached bool `json:"attached"`
}

// FinishedCommand is a command that ended, as history keeps it.
type FinishedCommand struct {
	ID              string       `json:"id"`
	RepoID          string       `json:"repoId"`
	NodeID          string       `json:"nodeId"`
	WorkspaceID     string       `json:"workspaceId"`
	Agent           string       `json:"agent"`
	Argv            string       `json:"argv"`
	Kind            string       `json:"kind"`
	State           CommandState `json:"state"`
	ExitCode        *int         `json:"exitCode"`
	Failure         *string      `json:"failure"` // compile, test, lint, signal, exit
	EndedAt         int64        `json:"endedAt"` // unix ms
	QueuedSeconds   float64      `json:"queuedSeconds"`
	Seconds         float64      `json:"seconds"`
	ColdSeconds     *float64     `json:"coldSeconds"`
	Rebuilt         *int         `json:"rebuilt"`
	Of              *int         `json:"of"`
	PeakMemoryBytes int64        `json:"peakMemoryBytes"`
	Parks           int          `json:"parks"`
}

// ---- requests ----------------------------------------------------------

// CreateWorkspace asks for a fork of a repo's newest warm base.
type CreateWorkspace struct {
	Repo   string `json:"repo"`
	Agent  string `json:"agent"`
	Branch string `json:"branch,omitempty"`
	// Node pins the workspace to a node (host name); otherwise the
	// controller picks the one with a warm base and the most free memory.
	Node string `json:"node,omitempty"`
}

// RunCommand asks a workspace to run a command.
type RunCommand struct {
	Args     []string      `json:"args"`
	Env      []string      `json:"env,omitempty"` // KEY=VALUE
	Kind     string        `json:"kind,omitempty"`
	Limits   CommandLimits `json:"limits"`
	Attached bool          `json:"attached"`
	// Port is the port a dev server listens on, for its URL.
	Port int `json:"port,omitempty"`
}

// SyncResult is what a sync did.
type SyncResult struct {
	Workspace    string `json:"workspace"`
	Head         string `json:"head"`
	Tree         string `json:"tree"`
	FilesChanged int    `json:"filesChanged"`
	Skipped      bool   `json:"skipped"` // the tree was already there
	BundleBytes  int64  `json:"bundleBytes"`
}
