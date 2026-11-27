package vm

import "golang.org/x/sys/unix"

// setThreadAffinity runs thread tid on CPU cpu alone.
func setThreadAffinity(tid, cpu int) error {
	var set unix.CPUSet
	set.Set(cpu)
	return unix.SchedSetaffinity(tid, &set)
}
