// Package api is the HTTP API that hotbed nodes and the hotbed client speak:
// the requests and replies, the errors, and a client for the controller.
package api

import "time"

const (
	// DefaultPort is the controller's port when an address names none.
	DefaultPort = 7120
	// WorkerPort is where workers serve the controller.
	WorkerPort = 7121
)

// ReportInterval is how often a worker reports to the controller, at the
// latest; it reports sooner when something changes. The controller counts a
// worker online while its last report is at most OnlineWindow old.
const (
	ReportInterval = 5 * time.Second
	OnlineWindow   = 3 * ReportInterval
)

// Resources is what a worker has, as it measured itself.
type Resources struct {
	Arch        string `json:"arch"`
	CPUs        int    `json:"cpus"`
	MemoryBytes int64  `json:"memoryBytes"`
	SwapBytes   int64  `json:"swapBytes"`
	Kernel      string `json:"kernel"`
	Machine     string `json:"machine"`
}

// Node is a worker as the controller knows it. Its identity (ID, Hostname,
// Login) comes from the tailnet, never from what the worker says about itself.
type Node struct {
	ID        string     `json:"id"`
	Hostname  string     `json:"hostname"`
	Login     string     `json:"login"`
	JoinedAt  time.Time  `json:"joinedAt"`
	LastSeen  *time.Time `json:"lastSeen,omitempty"`
	Online    bool       `json:"online"`
	Resources *Resources `json:"resources,omitempty"`
}

// JoinResponse is the controller's reply to a node that joined.
type JoinResponse struct {
	ClusterID string `json:"clusterId"`
	Node      Node   `json:"node"`
}

// NodesResponse lists the cluster's workers.
type NodesResponse struct {
	ClusterID string `json:"clusterId"`
	Nodes     []Node `json:"nodes"`
}

// Report is a worker's periodic account of itself. Finished commands and
// events carry sequence numbers; the worker sends each until the controller
// acknowledges it, and the controller ignores ones it already has.
type Report struct {
	ClusterID string    `json:"clusterId"`
	Resources Resources `json:"resources"`
	Usage     NodeUsage `json:"usage"`
	DiskBytes int64     `json:"diskBytes"`
	Disk      DiskUsage `json:"disk"`
	Reserved  int64     `json:"reservedMemoryBytes"`
	// OOMKills is the kernel's count of OOM kills since boot; hotbed parks
	// instead of killing, so it should not move.
	OOMKills   int64                        `json:"oomKills"`
	Storage    string                       `json:"storage"` // btrfs, zfs
	Bases      []Base                       `json:"bases"`
	Workspaces []Workspace                  `json:"workspaces"`
	Commands   []Command                    `json:"commands"`
	Finished   []Sequenced[FinishedCommand] `json:"finished"`
	Events     []Sequenced[WorkerEvent]     `json:"events"`
}

// Sequenced is an item a worker numbered so its delivery can be acknowledged.
type Sequenced[T any] struct {
	Seq  int64 `json:"seq"`
	Item T     `json:"item"`
}

// WorkerEvent is something that happened on a worker, for the cluster's log.
type WorkerEvent struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"` // fork, release, admit, finish, fail, park, thaw, base, node
	Text string    `json:"text"`
	Ref  *EventRef `json:"ref,omitempty"`
}

// ReportReply acknowledges a report and tells the worker what to host.
type ReportReply struct {
	Ack   int64  `json:"ack"`   // every sequence number up to this one is stored
	Repos []Repo `json:"repos"` // the repos this worker keeps bases of
}
