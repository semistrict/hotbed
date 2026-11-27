//go:build linux

package guestd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDelegateCgroup(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cmd-1")
	leaf := filepath.Join(root, "main")
	chowned := map[string][2]int{}
	delegateCgroup(root, leaf, 1000, 1000, func(path string, uid, gid int) error {
		chowned[path] = [2]int{uid, gid}
		return nil
	})

	for _, want := range []string{
		root,
		filepath.Join(root, "cgroup.procs"),
		filepath.Join(root, "cgroup.subtree_control"),
		filepath.Join(root, "memory.reclaim"),
		leaf,
		filepath.Join(leaf, "cgroup.procs"),
		filepath.Join(leaf, "cgroup.subtree_control"),
		filepath.Join(leaf, "memory.reclaim"),
		filepath.Join(leaf, "memory.high"),
		filepath.Join(leaf, "memory.max"),
	} {
		if got, ok := chowned[want]; !ok || got != [2]int{1000, 1000} {
			t.Errorf("expected %s chowned to 1000:1000, got %v (present=%v)", want, got, ok)
		}
	}
	// Root's own limits stay root-owned (matching systemd Delegate=yes).
	for _, keepRoot := range []string{
		filepath.Join(root, "memory.high"),
		filepath.Join(root, "memory.low"),
		filepath.Join(root, "cpu.weight"),
	} {
		if _, ok := chowned[keepRoot]; ok {
			t.Errorf("root limit file %s should not be chowned", keepRoot)
		}
	}
}

func TestRemoveCgroupTree(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cmd-tree")
	for _, sub := range []string{
		filepath.Join(root, "main"),
		filepath.Join(root, "cladefsd"),
		filepath.Join(root, "vm-1", "nested"),
	} {
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := removeCgroupTree(root); err != nil {
		t.Fatalf("removeCgroupTree: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("expected %s to be removed, stat err = %v", root, err)
	}
}
