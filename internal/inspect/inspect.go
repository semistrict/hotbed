// Package inspect provides opt-in, on-demand Linux process inspection (/proc
// and cgroup v2 snapshots) and active tracing (strace, perf, thread stack
// dumps, and curated bpftrace probes) scoped strictly to one hotbed command's
// cgroup. Nothing runs unless explicitly requested by a client.
package inspect

import (
	"bufio"
	"context"
	"encoding/hex"
	"fmt"
	"net/netip"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/semistrict/hotbed/internal/api"
	"github.com/semistrict/hotbed/internal/flame"
	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// Target identifies a live hotbed command on this worker.
type Target struct {
	CommandID string
	UnitName  string
	Cgroup    string // path under /sys/fs/cgroup (may be empty for non-cgroup fake units)
	MainPID   int
	Hostname  string
	Kernel    string
	Arch      string
	// ProfileWindow optionally queries the worker's always-on OTel eBPF
	// continuous profiler over [fromMs, toMs] so SampleCommand can include
	// multi-language on-CPU stacks at zero extra overhead.
	ProfileWindow func(fromMs, toMs int64) *pb.CommandProfile
}

// Emit streams one chunk of an active trace back to the caller.
type Emit func(*pb.TraceCommandChunk) error

// SampleEmit streams progress and the final report of a SampleCommand run.
type SampleEmit func(*pb.SampleCommandChunk) error

// Inspector performs opt-in inspection, sampling, and active tracing of a command's processes.
type Inspector interface {
	Inspect(ctx context.Context, target Target, req *pb.InspectCommandRequest) (*pb.InspectCommandResponse, error)
	Trace(ctx context.Context, target Target, req *pb.TraceCommandRequest, emit Emit) error
	Sample(ctx context.Context, target Target, req *pb.SampleCommandRequest, emit SampleEmit) error
	Capabilities() []*pb.ToolCapability
}

// filterPattern restricts strace -e filter expressions to safe characters.
var filterPattern = regexp.MustCompile(`^[a-zA-Z0-9_,!%@./-]+$`)

// cgroupPathPattern restricts Linux cgroup v2 paths interpolated into bpftrace
// programs to a strict safe character allowlist (no quotes, backslashes,
// whitespace, or control characters).
var cgroupPathPattern = regexp.MustCompile(`^/[A-Za-z0-9_./:@-]+$`)

// ValidateFilter rejects unsafe strace -e filter expressions.
func ValidateFilter(filter string) error {
	if filter == "" {
		return nil
	}
	if len(filter) > 256 || !filterPattern.MatchString(filter) {
		return fmt.Errorf("invalid syscall filter %q: use names or classes like file, network, process, or openat,read,write", filter)
	}
	return nil
}

// validateCgroupPath rejects empty, relative, out-of-cgroupfs, traversal, or
// unsafe cgroup paths before they are interpolated into a bpftrace program.
func validateCgroupPath(cgPath string) error {
	if cgPath == "" {
		return fmt.Errorf("empty cgroup path")
	}
	if len(cgPath) > 512 {
		return fmt.Errorf("cgroup path too long")
	}
	if !strings.HasPrefix(cgPath, "/sys/fs/cgroup/") {
		return fmt.Errorf("invalid cgroup path %q: must be under /sys/fs/cgroup/", cgPath)
	}
	if !cgroupPathPattern.MatchString(cgPath) {
		return fmt.Errorf("invalid cgroup path %q: contains disallowed characters", cgPath)
	}
	for _, seg := range strings.Split(cgPath[1:], "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("invalid cgroup path %q: contains unsafe path segment", cgPath)
		}
	}
	return nil
}

// gdbBacktraceArgs builds hardened non-interactive gdb arguments for capturing
// thread backtraces of pid as root. Passing -nx and -iex "set auto-load off"
// before -batch/-p prevents gdb from loading any .gdbinit, early-init, or
// target/workspace auto-load Python/GDB scripts.
func gdbBacktraceArgs(pid, maxFrames int) []string {
	bt := "thread apply all bt"
	if maxFrames > 0 {
		bt = fmt.Sprintf("thread apply all bt %d", maxFrames)
	}
	return []string{
		"-nx",
		"-iex", "set auto-load off",
		"-batch",
		"-ex", "set pagination off",
		"-ex", bt,
		"-p", strconv.Itoa(pid),
	}
}

// sanitizedTracerEnv returns a copy of env with HOME and XDG_CONFIG_HOME
// pointed at /nonexistent so root-run tracers like gdb never read user or
// workspace init files.
func sanitizedTracerEnv(env []string) []string {
	out := make([]string, 0, len(env)+2)
	for _, kv := range env {
		if strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "XDG_CONFIG_HOME=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "HOME=/nonexistent", "XDG_CONFIG_HOME=/nonexistent")
}

// ClampDuration bounds an active trace duration in seconds to [1, 60] (default 5).
func ClampDuration(sec int32) int {
	if sec <= 0 {
		return 5
	}
	if sec > 60 {
		return 60
	}
	return int(sec)
}

// ClampFrequency bounds a perf sampling frequency in Hz to [10, 999] (default 99).
func ClampFrequency(hz int32) int {
	if hz <= 0 {
		return 99
	}
	if hz < 10 {
		return 10
	}
	if hz > 999 {
		return 999
	}
	return int(hz)
}

// WantSection reports whether req asks for section s (empty sections means all).
func WantSection(req *pb.InspectCommandRequest, s pb.InspectSection) bool {
	if len(req.GetSections()) == 0 {
		return true
	}
	return slices.Contains(req.Sections, s)
}

// ---- /proc & cgroup v2 pure parsers ----------------------------------------

// ProcStat holds fields parsed from /proc/<pid>/stat.
type ProcStat struct {
	PID       int32
	Comm      string
	State     string
	PPID      int32
	MinFlt    int64
	MajFlt    int64
	UTime     int64 // clock ticks
	STime     int64 // clock ticks
	Threads   int32
	VSize     int64 // bytes
	RSS       int64 // pages
	Processor int32 // last CPU core executed on (-1 if unavailable)
}

// ParseProcStat parses a /proc/<pid>/stat line, handling spaces and parens in comm.
func ParseProcStat(line string) (ProcStat, error) {
	line = strings.TrimSpace(line)
	open := strings.IndexByte(line, '(')
	closeIdx := strings.LastIndexByte(line, ')')
	if open < 0 || closeIdx <= open || closeIdx+2 >= len(line) {
		return ProcStat{}, fmt.Errorf("malformed proc stat line")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line[:open]))
	if err != nil {
		return ProcStat{}, err
	}
	comm := line[open+1 : closeIdx]
	fields := strings.Fields(line[closeIdx+2:])
	// fields[0] is field #3 (state), fields[1] is #4 (ppid), etc.
	if len(fields) < 22 {
		return ProcStat{}, fmt.Errorf("short proc stat: %d fields", len(fields))
	}
	ppid, _ := strconv.Atoi(fields[1])
	minflt, _ := strconv.ParseInt(fields[7], 10, 64) // field #10
	majflt, _ := strconv.ParseInt(fields[9], 10, 64) // field #12
	utime, _ := strconv.ParseInt(fields[11], 10, 64) // field #14
	stime, _ := strconv.ParseInt(fields[12], 10, 64) // field #15
	threads, _ := strconv.Atoi(fields[17])           // field #20
	vsize, _ := strconv.ParseInt(fields[20], 10, 64) // field #23
	rss, _ := strconv.ParseInt(fields[21], 10, 64)   // field #24
	cpuCore := int32(-1)
	if len(fields) >= 37 {
		if c, err := strconv.Atoi(fields[36]); err == nil { // field #39
			cpuCore = int32(c)
		}
	}
	return ProcStat{
		PID:       int32(pid),
		Comm:      comm,
		State:     fields[0],
		PPID:      int32(ppid),
		MinFlt:    minflt,
		MajFlt:    majflt,
		UTime:     utime,
		STime:     stime,
		Threads:   int32(threads),
		VSize:     vsize,
		RSS:       rss,
		Processor: cpuCore,
	}, nil
}

// ProcStatus holds selected fields parsed from /proc/<pid>/status.
type ProcStatus struct {
	VmRSSBytes               int64
	VmSizeBytes              int64
	CapEff                   string
	Seccomp                  string
	NoNewPrivs               bool
	VoluntaryCtxtSwitches    int64
	NonvoluntaryCtxtSwitches int64
}

// ParseProcStatus parses key: value lines from /proc/<pid>/status.
func ParseProcStatus(content string) ProcStatus {
	var st ProcStatus
	for _, line := range strings.Split(content, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "VmRSS":
			st.VmRSSBytes = parseKiB(v)
		case "VmSize":
			st.VmSizeBytes = parseKiB(v)
		case "CapEff":
			st.CapEff = v
		case "Seccomp":
			switch v {
			case "0":
				st.Seccomp = "disabled"
			case "1":
				st.Seccomp = "strict"
			case "2":
				st.Seccomp = "filter"
			default:
				st.Seccomp = v
			}
		case "NoNewPrivs":
			st.NoNewPrivs = v == "1"
		case "voluntary_ctxt_switches":
			st.VoluntaryCtxtSwitches, _ = strconv.ParseInt(v, 10, 64)
		case "nonvoluntary_ctxt_switches":
			st.NonvoluntaryCtxtSwitches, _ = strconv.ParseInt(v, 10, 64)
		}
	}
	return st
}

// ParseProcIO parses read_bytes and write_bytes from /proc/<pid>/io.
func ParseProcIO(content string) (readBytes, writeBytes int64) {
	for _, line := range strings.Split(content, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "read_bytes":
			readBytes, _ = strconv.ParseInt(v, 10, 64)
		case "write_bytes":
			writeBytes, _ = strconv.ParseInt(v, 10, 64)
		}
	}
	return readBytes, writeBytes
}

// ParseProcCmdline converts NUL-separated /proc/<pid>/cmdline into a readable string.
func ParseProcCmdline(b []byte) string {
	b = []byte(strings.TrimRight(string(b), "\x00"))
	if len(b) == 0 {
		return ""
	}
	parts := strings.Split(string(b), "\x00")
	return strings.Join(parts, " ")
}

// ClassifyFDTarget classifies a readlink result from /proc/<pid>/fd/<n>.
func ClassifyFDTarget(target string) (kind string, inode uint64) {
	switch {
	case strings.HasPrefix(target, "socket:[") && strings.HasSuffix(target, "]"):
		ino, _ := strconv.ParseUint(target[8:len(target)-1], 10, 64)
		return "socket", ino
	case strings.HasPrefix(target, "pipe:[") && strings.HasSuffix(target, "]"):
		ino, _ := strconv.ParseUint(target[6:len(target)-1], 10, 64)
		return "pipe", ino
	case strings.HasPrefix(target, "anon_inode:"):
		return "anon_inode", 0
	case strings.HasSuffix(target, " (deleted)"):
		return "deleted", 0
	case strings.HasPrefix(target, "/dev/"):
		return "device", 0
	case strings.HasPrefix(target, "/"):
		return "file", 0
	default:
		return "other", 0
	}
}

// SocketInfo describes a socket indexed by inode from /proc/net/{tcp,tcp6,udp,udp6,unix}.
type SocketInfo struct {
	Proto   string
	State   string
	Local   string
	Remote  string
	TxQueue int64
	RxQueue int64
}

var tcpStates = map[string]string{
	"01": "ESTABLISHED",
	"02": "SYN_SENT",
	"03": "SYN_RECV",
	"04": "FIN_WAIT1",
	"05": "FIN_WAIT2",
	"06": "TIME_WAIT",
	"07": "CLOSE",
	"08": "CLOSE_WAIT",
	"09": "LAST_ACK",
	"0A": "LISTEN",
	"0B": "CLOSING",
	"0C": "NEW_SYN_RECV",
}

// ParseProcNetInet parses /proc/net/{tcp,tcp6,udp,udp6} into an inode map.
func ParseProcNetInet(proto, content string, out map[uint64]SocketInfo) {
	isV6 := strings.HasSuffix(proto, "6")
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		if i == 0 {
			continue // header
		}
		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}
		inode, err := strconv.ParseUint(fields[9], 10, 64)
		if err != nil || inode == 0 {
			continue
		}
		local := parseHexAddrPort(fields[1], isV6)
		remote := parseHexAddrPort(fields[2], isV6)
		state := tcpStates[strings.ToUpper(fields[3])]
		if state == "" {
			state = fields[3]
		}
		var txQ, rxQ int64
		if txHex, rxHex, ok := strings.Cut(fields[4], ":"); ok {
			if v, err := strconv.ParseInt(txHex, 16, 64); err == nil {
				txQ = v
			}
			if v, err := strconv.ParseInt(rxHex, 16, 64); err == nil {
				rxQ = v
			}
		}
		out[inode] = SocketInfo{
			Proto:   proto,
			State:   state,
			Local:   local,
			Remote:  remote,
			TxQueue: txQ,
			RxQueue: rxQ,
		}
	}
}

// ParseProcNetUnix parses /proc/net/unix into an inode map.
func ParseProcNetUnix(content string, out map[uint64]SocketInfo) {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		if i == 0 {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 7 {
			continue
		}
		inode, err := strconv.ParseUint(fields[6], 10, 64)
		if err != nil || inode == 0 {
			continue
		}
		path := ""
		if len(fields) >= 8 {
			path = fields[7]
		}
		state := "UNCONNECTED"
		switch fields[5] {
		case "01":
			state = "LISTEN"
		case "02":
			state = "CONNECTING"
		case "03":
			state = "CONNECTED"
		case "04":
			state = "DISCONNECTING"
		}
		out[inode] = SocketInfo{
			Proto: "unix",
			State: state,
			Local: path,
		}
	}
}

func parseHexAddrPort(hexPair string, isV6 bool) string {
	addrHex, portHex, ok := strings.Cut(hexPair, ":")
	if !ok {
		return hexPair
	}
	port, err := strconv.ParseUint(portHex, 16, 16)
	if err != nil {
		return hexPair
	}
	raw, err := hex.DecodeString(addrHex)
	if err != nil {
		return hexPair
	}
	if !isV6 && len(raw) == 4 {
		// Linux /proc/net/tcp writes IPv4 in host little-endian 32-bit word.
		ip := netip.AddrFrom4([4]byte{raw[3], raw[2], raw[1], raw[0]})
		return netip.AddrPortFrom(ip, uint16(port)).String()
	}
	if isV6 && len(raw) == 16 {
		// Linux /proc/net/tcp6 writes four 32-bit little-endian words.
		var b [16]byte
		for w := 0; w < 4; w++ {
			b[w*4+0] = raw[w*4+3]
			b[w*4+1] = raw[w*4+2]
			b[w*4+2] = raw[w*4+1]
			b[w*4+3] = raw[w*4+0]
		}
		ip := netip.AddrFrom16(b)
		return netip.AddrPortFrom(ip, uint16(port)).String()
	}
	return hexPair
}

// ParseKeyValueInt parses "key value" lines (cgroup cpu.stat, memory.stat).
func ParseKeyValueInt(content string) map[string]int64 {
	m := map[string]int64{}
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			if v, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
				m[fields[0]] = v
			}
		}
	}
	return m
}

// parsePSIAll parses avg10 and cumulative microsecond stall total= from cgroup pressure files.
func parsePSIAll(content string) (someAvg10, fullAvg10 float64, someTotal, fullTotal int64) {
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		var avg10 float64
		var total int64
		for _, f := range fields[1:] {
			if v, ok := strings.CutPrefix(f, "avg10="); ok {
				avg10, _ = strconv.ParseFloat(v, 64)
			} else if v, ok := strings.CutPrefix(f, "total="); ok {
				total, _ = strconv.ParseInt(v, 10, 64)
			}
		}
		switch fields[0] {
		case "some":
			someAvg10, someTotal = avg10, total
		case "full":
			fullAvg10, fullTotal = avg10, total
		}
	}
	return someAvg10, fullAvg10, someTotal, fullTotal
}

// ParseCgroupIO parses cgroup v2 io.stat lines: "259:0 rbytes=123 wbytes=456 rios=1 wios=2 ...".
func ParseCgroupIO(content string) []*pb.CgroupIO {
	var out []*pb.CgroupIO
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		entry := &pb.CgroupIO{Device: fields[0]}
		for _, f := range fields[1:] {
			k, v, ok := strings.Cut(f, "=")
			if !ok {
				continue
			}
			n, _ := strconv.ParseInt(v, 10, 64)
			switch k {
			case "rbytes":
				entry.Rbytes = n
			case "wbytes":
				entry.Wbytes = n
			case "rios":
				entry.Rios = n
			case "wios":
				entry.Wios = n
			}
		}
		out = append(out, entry)
	}
	return out
}

// ParseKernelStack parses /proc/<pid>/task/<tid>/stack into clean symbol frames.
func ParseKernelStack(content string) []string {
	var frames []string
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Lines look like "[<0>] futex_wait_queue+0x60/0xa0"
		if strings.HasPrefix(line, "[<") {
			if idx := strings.Index(line, ">]"); idx >= 0 {
				line = strings.TrimSpace(line[idx+2:])
			}
		}
		if line != "" {
			frames = append(frames, line)
		}
	}
	return frames
}

// ParseSmapsRollup parses /proc/<pid>/smaps_rollup into a ProcessMemoryMap summary.
func ParseSmapsRollup(pid int32, comm, content string) *pb.ProcessMemoryMap {
	m := &pb.ProcessMemoryMap{Pid: pid, Comm: comm}
	for _, line := range strings.Split(content, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		bytesVal := parseKiB(strings.TrimSpace(v))
		switch k {
		case "Rss":
			m.RssBytes = bytesVal
		case "Pss":
			m.PssBytes = bytesVal
		case "Private_Dirty":
			m.PrivateDirtyBytes = bytesVal
		case "Shared_Clean":
			m.SharedCleanBytes = bytesVal
		case "Swap":
			m.SwapBytes = bytesVal
		case "Anonymous":
			m.AnonBytes = bytesVal
		case "Shmem":
			m.ShmemBytes = bytesVal
		}
	}
	return m
}

// ParseProcMaps aggregates /proc/<pid>/maps by path+perms and returns the largest topN regions.
func ParseProcMaps(content string, topN int) []*pb.MemoryRegion {
	type key struct{ path, perms string }
	agg := map[key]*pb.MemoryRegion{}
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		startHex, endHex, ok := strings.Cut(fields[0], "-")
		if !ok {
			continue
		}
		start, err1 := strconv.ParseUint(startHex, 16, 64)
		end, err2 := strconv.ParseUint(endHex, 16, 64)
		if err1 != nil || err2 != nil || end <= start {
			continue
		}
		perms := fields[1]
		path := "[anon]"
		if len(fields) >= 6 {
			path = strings.Join(fields[5:], " ")
		}
		k := key{path: path, perms: perms}
		r := agg[k]
		if r == nil {
			r = &pb.MemoryRegion{Perms: perms, Path: path}
			agg[k] = r
		}
		r.SizeBytes += int64(end - start)
	}
	out := make([]*pb.MemoryRegion, 0, len(agg))
	for _, r := range agg {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SizeBytes != out[j].SizeBytes {
			return out[i].SizeBytes > out[j].SizeBytes
		}
		return out[i].Path < out[j].Path
	})
	if topN > 0 && len(out) > topN {
		out = out[:topN]
	}
	return out
}

// ParseProcLimits parses /proc/<pid>/limits into ResourceLimit items.
func ParseProcLimits(content string) []*pb.ResourceLimit {
	var out []*pb.ResourceLimit
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		if i == 0 || strings.TrimSpace(line) == "" {
			continue // header
		}
		// Columns are separated by 2+ spaces: Limit, Soft Limit, Hard Limit, [Units].
		parts := multiSpaceSplit(strings.TrimSpace(line))
		if len(parts) < 3 {
			continue
		}
		item := &pb.ResourceLimit{
			Name: parts[0],
			Soft: parts[1],
			Hard: parts[2],
		}
		if len(parts) >= 4 {
			item.Units = parts[3]
		}
		out = append(out, item)
	}
	return out
}

func multiSpaceSplit(s string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(s); {
		if i+1 < len(s) && s[i] == ' ' && s[i+1] == ' ' {
			if start < i {
				parts = append(parts, strings.TrimSpace(s[start:i]))
			}
			for i < len(s) && s[i] == ' ' {
				i++
			}
			start = i
		} else {
			i++
		}
	}
	if start < len(s) {
		parts = append(parts, strings.TrimSpace(s[start:]))
	}
	return parts
}

func parseKiB(s string) int64 {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0
	}
	n, _ := strconv.ParseInt(fields[0], 10, 64)
	if len(fields) >= 2 && strings.EqualFold(fields[1], "kB") {
		return n * 1024
	}
	return n
}

// ---- Active trace output parsers -------------------------------------------

// ParseStraceSummary parses `strace -f -c` tabular output.
func ParseStraceSummary(output string) []*pb.SyscallStat {
	var out []*pb.SyscallStat
	sc := bufio.NewScanner(strings.NewReader(output))
	inTable := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "------") {
			inTable = !inTable
			continue
		}
		if !inTable || line == "" {
			continue
		}
		fields := strings.Fields(line)
		// Columns: % time | seconds | usecs/call | calls | [errors] | syscall
		if len(fields) < 5 {
			continue
		}
		pct, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			continue
		}
		secs, _ := strconv.ParseFloat(fields[1], 64)
		usecs, _ := strconv.ParseInt(fields[2], 10, 64)
		calls, _ := strconv.ParseInt(fields[3], 10, 64)
		var errs int64
		var name string
		if len(fields) >= 6 {
			errs, _ = strconv.ParseInt(fields[4], 10, 64)
			name = fields[5]
		} else {
			name = fields[4]
		}
		if name == "total" {
			continue
		}
		out = append(out, &pb.SyscallStat{
			Name:         name,
			PctTime:      pct,
			Seconds:      secs,
			UsecsPerCall: usecs,
			Calls:        calls,
			Errors:       errs,
		})
	}
	return out
}

func isPerfNumeric(s string) bool {
	if s == "" {
		return false
	}
	if s[0] < '0' || s[0] > '9' || s[len(s)-1] < '0' || s[len(s)-1] > '9' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && c != ',' && c != '.' {
			return false
		}
	}
	return true
}

func isPerfUnit(s string) bool {
	return s == "msec" || s == "sec" || s == "MB" || s == "K/sec" || s == "M/sec"
}

// ParsePerfStat parses `perf stat` human-readable output into structured counters.
func ParsePerfStat(output string) []*pb.PerfCounter {
	var out []*pb.PerfCounter
	sc := bufio.NewScanner(strings.NewReader(output))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "Performance counter") || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasSuffix(line, "seconds time elapsed") {
			fields := strings.Fields(line)
			if len(fields) >= 1 && isPerfNumeric(fields[0]) {
				out = append(out, &pb.PerfCounter{Name: "time-elapsed", Value: fields[0], Unit: "seconds"})
			}
			// Everything after the elapsed-time footer (seconds user/sys,
			// nmi_watchdog advice, etc.) is not a counter row.
			break
		}
		if strings.HasSuffix(line, "seconds user") || strings.HasSuffix(line, "seconds sys") {
			break
		}
		// Split optional "# note" comment.
		mainPart, note, _ := strings.Cut(line, "#")
		note = strings.TrimSpace(note)
		fields := strings.Fields(mainPart)
		if len(fields) < 2 {
			continue
		}
		val := fields[0]
		if val == "<not" {
			// "<not counted> <event> [(X.XX%)]" or "<not supported> <event> [(X.XX%)]"
			if len(fields) >= 3 && strings.HasSuffix(fields[1], ">") {
				unit := ""
				name := fields[2]
				if len(fields) >= 4 && isPerfUnit(fields[2]) && !strings.HasPrefix(fields[3], "(") {
					unit = fields[2]
					name = fields[3]
				}
				if !strings.HasPrefix(name, "(") {
					out = append(out, &pb.PerfCounter{
						Name:  name,
						Value: fields[0] + " " + fields[1],
						Unit:  unit,
						Note:  note,
					})
				}
			}
			continue
		}
		if !isPerfNumeric(val) {
			continue
		}
		// Check if field[1] is a unit (e.g., "msec") followed by the event name.
		unit := ""
		name := fields[1]
		if len(fields) >= 3 && isPerfUnit(fields[1]) && !strings.HasPrefix(fields[2], "(") {
			unit = fields[1]
			name = fields[2]
		}
		if strings.HasPrefix(name, "(") {
			continue
		}
		// Strip trailing :u/:k or cpu_core/.../ decoration lightly, keeping event readable.
		out = append(out, &pb.PerfCounter{
			Name:  name,
			Value: val,
			Unit:  unit,
			Note:  note,
		})
	}
	return out
}

// ParsePerfScript parses `perf script` output into flamegraph folded stacks,
// each rooted at its process's name, and top functions, both naming their
// frames by index in the frames it returns.
func ParsePerfScript(output string, limit int) ([]*pb.ProfileFrame, []*pb.FoldedStack, []*pb.ProfileFunction) {
	if limit <= 0 {
		limit = 200
	}
	var all flame.Table
	stackCounts := flame.Stacks{}
	var funcs flame.Functions
	var totalSamples int64

	var curComm string
	var curFrames []*pb.ProfileFrame

	flush := func() {
		if len(curFrames) == 0 {
			return
		}
		totalSamples++
		// perf script emits leaf-first; reverse to root->leaf for folded stacks.
		key := make([]byte, 0, 4*(len(curFrames)+1))
		if curComm != "" {
			key = flame.AppendID(key, all.ID(&pb.ProfileFrame{Name: curComm, FrameType: "process"}))
		}
		// The functions are the stack's own frames: its process is none.
		ids := make([]uint32, 0, len(curFrames))
		for i := len(curFrames) - 1; i >= 0; i-- {
			id := all.ID(curFrames[i])
			key = flame.AppendID(key, id)
			ids = append(ids, id)
		}
		funcs.Add(ids, 1)
		stackCounts[flame.StackKey{Frames: string(key), LeafType: curFrames[0].FrameType, Comm: curComm}]++
		curFrames = curFrames[:0]
	}

	sc := bufio.NewScanner(strings.NewReader(output))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		if !strings.HasPrefix(line, "\t") && !strings.HasPrefix(line, " ") {
			// Sample header line: "go 12345 1234.567890:   10000000 cpu-clock:ppp:"
			flush()
			fields := strings.Fields(line)
			if len(fields) > 0 {
				curComm = fields[0]
			}
			continue
		}
		// Frame line: "\tffffffff810a1234 func_name+0x14 ([kernel.kallsyms])"
		trimmed := strings.TrimSpace(line)
		fields := strings.Fields(trimmed)
		if len(fields) < 2 {
			continue
		}
		rawSym := fields[1]
		if plus := strings.LastIndex(rawSym, "+0x"); plus > 0 {
			rawSym = rawSym[:plus]
		}
		mod := ""
		if len(fields) >= 3 {
			mod = strings.Trim(fields[len(fields)-1], "()")
		}
		ft := "native"
		isBracketMod := strings.HasPrefix(mod, "[") && strings.HasSuffix(mod, "]")
		if strings.Contains(mod, "kernel") || strings.Contains(mod, "vmlinux") ||
			(isBracketMod && mod != "[unknown]" && mod != "[vdso]" && mod != "[vsyscall]") {
			ft = "kernel"
			rawSym = strings.TrimSuffix(rawSym, "_[k]")
		}
		if mod != "" && !isBracketMod {
			mod = filepath.Base(mod)
		}
		curFrames = append(curFrames, &pb.ProfileFrame{Name: rawSym, Module: mod, FrameType: ft})
	}
	flush()

	frames := all.Frames()
	stacks := stackCounts.Folded()
	stacks = stacks[:min(len(stacks), limit)]
	topFns := funcs.Top(frames, totalSamples)
	topFns = topFns[:min(len(topFns), limit)]

	// Only the frames the top stacks and functions name.
	var top flame.Table
	top.Rebase(frames, stacks, topFns)
	return top.Frames(), stacks, topFns
}

// buildBPFTraceProgram generates the cgroup-scoped (and optionally PID-filtered)
// bpftrace program and human-readable label for a curated BPF trace kind.
func buildBPFTraceProgram(kind pb.TraceKind, cgPath string, durSec int, pid int32) (label, prog string, err error) {
	return buildBPFTraceProgramForCgroups(kind, []string{cgPath}, durSec, pid)
}

// buildBPFTraceProgramForCgroups generates a bpftrace program scoped to one or
// more cgroup v2 paths (and optionally filtered to pid). Multiple paths are
// needed when a command's cgroup has nested sub-cgroups, because bpftrace's
// `cgroup` builtin returns the exact leaf cgroup ID rather than matching
// descendants.
func buildBPFTraceProgramForCgroups(kind pb.TraceKind, cgPaths []string, durSec int, pid int32) (label, prog string, err error) {
	if len(cgPaths) == 0 {
		return "", "", api.Errorf(api.CodeInvalidArgs, "empty cgroup path")
	}
	if durSec <= 0 {
		durSec = 1
	}
	terms := make([]string, 0, len(cgPaths))
	for _, cgPath := range cgPaths {
		if err := validateCgroupPath(cgPath); err != nil {
			return "", "", api.Errorf(api.CodeInvalidArgs, "%v", err)
		}
		terms = append(terms, fmt.Sprintf(`cgroup == cgroupid(%q)`, cgPath))
	}
	pred := terms[0]
	if len(terms) > 1 {
		pred = "(" + strings.Join(terms, " || ") + ")"
	}
	pidPred := pred
	offcpuPred := pred
	if pid > 0 {
		pidPred = fmt.Sprintf(`%s && pid == %d`, pred, pid)
		offcpuPred = fmt.Sprintf(`%s && (pid == %d || args.prev_pid == %d)`, pred, pid, pid)
	}
	switch kind {
	case pb.TraceKind_TRACE_KIND_BPF_EXECSNOOP:
		label = "execsnoop (process exec events)"
		prog = fmt.Sprintf(`tracepoint:syscalls:sys_enter_execve /%s/ { printf("%%-8d %%-16s ", pid, comm); join(args.argv); } interval:s:%d { exit(); }`, pidPred, durSec)
	case pb.TraceKind_TRACE_KIND_BPF_OPENSNOOP:
		label = "opensnoop (file open events)"
		prog = fmt.Sprintf(`tracepoint:syscalls:sys_enter_openat /%s/ { printf("%%-8d %%-16s %%s\n", pid, comm, str(args.filename)); } interval:s:%d { exit(); }`, pidPred, durSec)
	case pb.TraceKind_TRACE_KIND_BPF_VFSSTAT:
		label = "vfsstat (VFS read/write/open/fsync counts)"
		prog = fmt.Sprintf(`kprobe:vfs_read,kprobe:vfs_write,kprobe:vfs_open,kprobe:vfs_fsync /%s/ { @[probe, comm] = count(); } interval:s:%d { exit(); }`, pidPred, durSec)
	case pb.TraceKind_TRACE_KIND_BPF_OFFCPU:
		label = "offcpu (blocking / sleep stack traces)"
		prog = fmt.Sprintf(`tracepoint:sched:sched_switch /%s/ { @start[args.prev_pid] = nsecs; @kstack[args.prev_pid] = kstack(8); @comm[args.prev_pid] = comm; } tracepoint:sched:sched_switch /@start[args.next_pid]/ { $d = (nsecs - @start[args.next_pid]) / 1000; @[@kstack[args.next_pid], @comm[args.next_pid]] = sum($d); delete(@start[args.next_pid]); delete(@kstack[args.next_pid]); delete(@comm[args.next_pid]); } interval:s:%d { exit(); } END { clear(@start); clear(@kstack); clear(@comm); }`, offcpuPred, durSec)
	case pb.TraceKind_TRACE_KIND_BPF_NET:
		label = "tcp/net activity (connect, sendmsg, recvmsg)"
		prog = fmt.Sprintf(`kprobe:tcp_v4_connect,kprobe:tcp_v6_connect,kprobe:tcp_sendmsg,kprobe:tcp_recvmsg /%s/ { @[func, comm] = count(); } interval:s:%d { exit(); }`, pidPred, durSec)
	default:
		return "", "", api.Errorf(api.CodeInvalidArgs, "unsupported bpf trace kind %v", kind)
	}
	return label, prog, nil
}
