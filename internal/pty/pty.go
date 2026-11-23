// Package pty allocates and resizes pseudo-terminals on Linux and macOS.
package pty

import "os"

// Open allocates a new PTY master/slave pair and returns the master file,
// the slave file, and the slave device path (e.g. /dev/pts/3 or /dev/ttys003).
func Open() (master, slave *os.File, slavePath string, err error) {
	return openPTY()
}

// Resize sets the terminal window size of a PTY master.
func Resize(master *os.File, cols, rows uint16) error {
	return resizePTY(master, cols, rows)
}
