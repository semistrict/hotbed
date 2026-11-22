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

	pb "github.com/semistrict/hotbed/internal/gen/hotbed/v1"
)

// Target identifies a live hotbed command on this worker.
type Target struct {
	CommandID string
	UnitName  string
	Cgroup    string // path under /sys/fs/cgroup (may be empty for non-cgroup fake units)
	MainPID   int
}

// Emit streams one chunk of an active trace back to the caller.
type Emit func(*pb.TraceCommandChunk) error

// Inspector performs opt-in inspection and active tracing of a command's processes.
type Inspector interface {
	Inspect(ctx context.Context, target Target, req *pb.InspectCommandRequest) (*pb.InspectCommandResponse, error)
	Trace(ctx context.Context, target Target, req *pb.TraceCommandRequest, emit Emit) error
	Capabilities() []*pb.ToolCapability
}

// filterPattern restricts strace -e filter expressions to safe characters.
var filterPattern = regexp.MustCompile(`^[a-zA-Z0-9_,!%@./-]+$`)

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
	PID     int32
	Comm    string
	State   string
	PPID    int32
	UTime   int64 // clock ticks
	STime   int64 // clock ticks
	Threads int32
	VSize   int64 // bytes
	RSS     int64 // pages
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
	utime, _ := strconv.ParseInt(fields[11], 10, 64)   // field #14
	stime, _ := strconv.ParseInt(fields[12], 10, 64)   // field #15
	threads, _ := strconv.Atoi(fields[17])             // field #20
	vsize, _ := strconv.ParseInt(fields[20], 10, 64)   // field #23
	rss, _ := strconv.ParseInt(fields[21], 10, 64)     // field #24
	return ProcStat{
		PID:     int32(pid),
		Comm:    comm,
		State:   fields[0],
		PPID:    int32(ppid),
		UTime:   utime,
		STime:   stime,
		Threads: int32(threads),
		VSize:   vsize,
		RSS:     rss,
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

// ParseFDInfo extracts pos and open mode ("r", "w", "rw") from /proc/<pid>/fdinfo/<fd>.
func ParseFDInfo(content string) (pos int64, mode string) {
	for _, line := range strings.Split(content, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "pos":
			pos, _ = strconv.ParseInt(v, 10, 64)
		case "flags":
			// flags are octal; lower 2 bits are O_ACCMODE (0=O_RDONLY, 1=O_WRONLY, 2=O_RDWR).
			if flags, err := strconv.ParseUint(v, 8, 64); err == nil {
				switch flags & 3 {
				case 0:
					mode = "r"
				case 1:
					mode = "w"
				case 2:
					mode = "rw"
				}
			}
		}
	}
	return pos, mode
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
	Proto  string
	State  string
	Local  string
	Remote string
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
		out[inode] = SocketInfo{
			Proto:  proto,
			State:  state,
			Local:  local,
			Remote: remote,
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

// ParsePSI parses "some avg10=X ..." and "full avg10=Y ..." from cgroup pressure files.
func ParsePSI(content string) (someAvg10, fullAvg10 float64) {
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		var avg10 float64
		for _, f := range fields[1:] {
			if v, ok := strings.CutPrefix(f, "avg10="); ok {
				avg10, _ = strconv.ParseFloat(v, 64)
			}
		}
		switch fields[0] {
		case "some":
			someAvg10 = avg10
		case "full":
			fullAvg10 = avg10
		}
	}
	return someAvg10, fullAvg10
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
			if len(fields) >= 1 {
				out = append(out, &pb.PerfCounter{Name: "time-elapsed", Value: fields[0], Unit: "seconds"})
			}
			continue
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
			// "<not counted>" or "<not supported>"
			if len(fields) >= 3 {
				out = append(out, &pb.PerfCounter{
					Name:  fields[len(fields)-1],
					Value: fields[0] + " " + fields[1],
					Note:  note,
				})
			}
			continue
		}
		// Check if field[1] is a unit (e.g., "msec") followed by the event name.
		unit := ""
		name := fields[1]
		if len(fields) >= 3 && (fields[1] == "msec" || fields[1] == "sec" || fields[1] == "MB" || fields[1] == "K/sec" || fields[1] == "M/sec") {
			unit = fields[1]
			name = fields[2]
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

// ParsePerfScript parses `perf script` output into flamegraph folded stacks and top functions.
func ParsePerfScript(output string, limit int) ([]*pb.FoldedStack, []*pb.ProfileFunction) {
	if limit <= 0 {
		limit = 200
	}
	type stackKey struct {
		stack    string
		leafType string
		comm     string
	}
	stackCounts := map[stackKey]int64{}
	type fnMeta struct {
		module    string
		frameType string
		self      int64
		total     int64
	}
	funcs := map[string]*fnMeta{}
	var totalSamples int64

	var curComm string
	var curFrames []struct {
		name      string
		module    string
		frameType string
	}

	flush := func() {
		if len(curFrames) == 0 {
			return
		}
		totalSamples++
		// perf script emits leaf-first; reverse to root->leaf for folded stacks.
		names := make([]string, 0, len(curFrames)+1)
		if curComm != "" {
			names = append(names, curComm)
		}
		seenInStack := map[string]bool{}
		for i := len(curFrames) - 1; i >= 0; i-- {
			fr := curFrames[i]
			names = append(names, fr.name)
			m := funcs[fr.name]
			if m == nil {
				m = &fnMeta{module: fr.module, frameType: fr.frameType}
				funcs[fr.name] = m
			}
			if !seenInStack[fr.name] {
				m.total++
				seenInStack[fr.name] = true
			}
		}
		leaf := curFrames[0]
		if m := funcs[leaf.name]; m != nil {
			m.self++
		}
		sk := stackKey{
			stack:    strings.Join(names, ";"),
			leafType: leaf.frameType,
			comm:     curComm,
		}
		stackCounts[sk]++
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
		if strings.Contains(mod, "kernel") || strings.HasPrefix(mod, "[") {
			ft = "kernel"
		} else if mod != "" {
			mod = filepath.Base(mod)
		}
		curFrames = append(curFrames, struct {
			name      string
			module    string
			frameType string
		}{name: rawSym, module: mod, frameType: ft})
	}
	flush()

	stacks := make([]*pb.FoldedStack, 0, len(stackCounts))
	for k, cnt := range stackCounts {
		stacks = append(stacks, &pb.FoldedStack{
			Stack:    k.stack,
			Samples:  cnt,
			LeafType: k.leafType,
			Comm:     k.comm,
		})
	}
	sort.Slice(stacks, func(i, j int) bool {
		if stacks[i].Samples != stacks[j].Samples {
			return stacks[i].Samples > stacks[j].Samples
		}
		return stacks[i].Stack < stacks[j].Stack
	})
	if len(stacks) > limit {
		stacks = stacks[:limit]
	}

	topFns := make([]*pb.ProfileFunction, 0, len(funcs))
	for name, m := range funcs {
		var selfPct, totalPct float64
		if totalSamples > 0 {
			selfPct = 100 * float64(m.self) / float64(totalSamples)
			totalPct = 100 * float64(m.total) / float64(totalSamples)
		}
		topFns = append(topFns, &pb.ProfileFunction{
			Name:         name,
			Module:       m.module,
			FrameType:    m.frameType,
			SelfSamples:  m.self,
			TotalSamples: m.total,
			SelfPct:      selfPct,
			TotalPct:     totalPct,
		})
	}
	sort.Slice(topFns, func(i, j int) bool {
		if topFns[i].SelfSamples != topFns[j].SelfSamples {
			return topFns[i].SelfSamples > topFns[j].SelfSamples
		}
		if topFns[i].TotalSamples != topFns[j].TotalSamples {
			return topFns[i].TotalSamples > topFns[j].TotalSamples
		}
		return topFns[i].Name < topFns[j].Name
	})
	if len(topFns) > limit {
		topFns = topFns[:limit]
	}
	return stacks, topFns
}
