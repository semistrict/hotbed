package sysinfo

import (
	"strconv"
	"strings"
)

// ParseMemoryEventsOOM reports whether a cgroup's memory.events (content)
// records a kernel OOM kill in it: oom_kill or oom_group_kill above 0.
func ParseMemoryEventsOOM(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if fields[0] == "oom_kill" || fields[0] == "oom_group_kill" {
			if n, err := strconv.ParseInt(fields[1], 10, 64); err == nil && n > 0 {
				return true
			}
		}
	}
	return false
}
