package api

// The cockpit's view of the cluster: the shapes in ui/src/api/types.ts, which
// the UI was designed against. Times are RFC 3339 strings, or unix
// milliseconds where a chart needs numbers.

type NodeUsage struct {
	CPUBusy         float64 `json:"cpuBusy"`
	MemoryUsedBytes int64   `json:"memoryUsedBytes"`
	SwapUsedBytes   int64   `json:"swapUsedBytes"`
	CPUPressure     float64 `json:"cpuPressure"`
	MemoryPressure  float64 `json:"memoryPressure"`
}

type DiskUsage struct {
	BasesBytes          int64 `json:"basesBytes"`
	ForksExclusiveBytes int64 `json:"forksExclusiveBytes"`
	OtherBytes          int64 `json:"otherBytes"`
	FreeBytes           int64 `json:"freeBytes"`
}

type CockpitNode struct {
	ID                  string    `json:"id"`
	Hostname            string    `json:"hostname"`
	Arch                string    `json:"arch"`
	Machine             string    `json:"machine"`
	Kernel              string    `json:"kernel"`
	Online              bool      `json:"online"`
	JoinedAt            string    `json:"joinedAt"`
	LastSeen            string    `json:"lastSeen"`
	CPUs                int       `json:"cpus"`
	MemoryBytes         int64     `json:"memoryBytes"`
	SwapBytes           int64     `json:"swapBytes"`
	DiskBytes           int64     `json:"diskBytes"`
	Usage               NodeUsage `json:"usage"`
	ReservedMemoryBytes int64     `json:"reservedMemoryBytes"`
	Disk                DiskUsage `json:"disk"`
}

type CockpitRepo struct {
	ID               string  `json:"id"`
	Name             string  `json:"name"`
	Language         string  `json:"language"`
	Units            int     `json:"units"`
	ColdBuildSeconds float64 `json:"coldBuildSeconds"`
}

type Totals struct {
	SavedSeconds     float64 `json:"savedSeconds"`
	Builds           int     `json:"builds"`
	Forks            int     `json:"forks"`
	MedianForkMillis int64   `json:"medianForkMillis"`
	OOMKills         int     `json:"oomKills"`
	Parks            int     `json:"parks"`
}

type Sample struct {
	T              int64   `json:"t"`
	CPUBusy        float64 `json:"cpuBusy"`
	MemoryUsed     float64 `json:"memoryUsed"`
	MemoryPressure float64 `json:"memoryPressure"`
}

type LoadSample struct {
	T       int64 `json:"t"`
	Running int   `json:"running"`
	Queued  int   `json:"queued"`
	Parked  int   `json:"parked"`
}

type HourBucket struct {
	Hour                int64   `json:"hour"`
	Builds              int     `json:"builds"`
	SpentSeconds        float64 `json:"spentSeconds"`
	SavedSeconds        float64 `json:"savedSeconds"`
	Forks               int     `json:"forks"`
	Parks               int     `json:"parks"`
	QueueWaitP95Seconds float64 `json:"queueWaitP95Seconds"`
}

type NodeHour struct {
	Hour       int64   `json:"hour"`
	CPUBusy    float64 `json:"cpuBusy"`
	MemoryUsed float64 `json:"memoryUsed"`
}

type ClusterEvent struct {
	ID     int64     `json:"id"`
	At     string    `json:"at"`
	Kind   string    `json:"kind"`
	NodeID *string   `json:"nodeId"`
	Text   string    `json:"text"`
	Ref    *EventRef `json:"ref"`
}

type EventRef struct {
	Type string `json:"type"` // command, workspace, node
	ID   string `json:"id"`
}

type Snapshot struct {
	ClusterID     string                `json:"clusterId"`
	Controller    string                `json:"controller"`
	Now           string                `json:"now"`
	SampleSeconds int                   `json:"sampleSeconds"`
	Nodes         []CockpitNode         `json:"nodes"`
	Repos         []CockpitRepo         `json:"repos"`
	Bases         []Base                `json:"bases"`
	Workspaces    []Workspace           `json:"workspaces"`
	Commands      []Command             `json:"commands"`
	Events        []ClusterEvent        `json:"events"`
	Totals24h     Totals                `json:"totals24h"`
	Series        map[string][]Sample   `json:"series"`
	Load          []LoadSample          `json:"load"`
	Hourly        []HourBucket          `json:"hourly"`
	NodeHourly    map[string][]NodeHour `json:"nodeHourly"`
	History       []FinishedCommand     `json:"history"`
}

type CommandLog struct {
	CommandID string   `json:"commandId"`
	Lines     []string `json:"lines"`
}

type FileEntry struct {
	Path      string  `json:"path"`
	Kind      string  `json:"kind"` // file, dir
	SizeBytes int64   `json:"sizeBytes"`
	Change    *string `json:"change"` // added, modified, or null
}

type FileContent struct {
	Path         string `json:"path"`
	Text         string `json:"text"`
	ChangedLines []int  `json:"changedLines"`
}
