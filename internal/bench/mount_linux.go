package bench

import "golang.org/x/sys/unix"

func mountResctrl(target string) error { return unix.Mount("resctrl", target, "resctrl", 0, "") }
