// Package api is the HTTP API that hotbed nodes and the hotbed client speak:
// the requests and replies, the errors, and a client for the controller.
package api

import "time"

// DefaultPort is the controller's port when an address names none.
const DefaultPort = 7120

// HeartbeatInterval is how often a worker reports to the controller. The
// controller counts a worker online while its last report is at most
// OnlineWindow old, which tolerates two missed reports.
const (
	HeartbeatInterval = 5 * time.Second
	OnlineWindow      = 3 * HeartbeatInterval
)

// Resources is what a worker has to offer, as it measured itself.
type Resources struct {
	Arch        string `json:"arch"`
	CPUs        int    `json:"cpus"`
	MemoryBytes uint64 `json:"memory_bytes"`
	Kernel      string `json:"kernel"`
}

// Node is a worker as the controller knows it. Its identity (ID, Hostname,
// Login) comes from the tailnet, never from what the worker says about itself.
type Node struct {
	ID        string     `json:"id"`
	Hostname  string     `json:"hostname"`
	Login     string     `json:"login"`
	JoinedAt  time.Time  `json:"joined_at"`
	LastSeen  *time.Time `json:"last_seen,omitempty"`
	Online    bool       `json:"online"`
	Resources *Resources `json:"resources,omitempty"`
}

// JoinResponse is the controller's reply to a node that joined.
type JoinResponse struct {
	ClusterID string `json:"cluster_id"`
	Node      Node   `json:"node"`
}

// Heartbeat is a worker's periodic report. ClusterID guards against a worker
// reporting to a controller that was initialized again since it joined.
type Heartbeat struct {
	ClusterID string    `json:"cluster_id"`
	Resources Resources `json:"resources"`
}

// NodesResponse lists the cluster's workers.
type NodesResponse struct {
	ClusterID string `json:"cluster_id"`
	Nodes     []Node `json:"nodes"`
}
