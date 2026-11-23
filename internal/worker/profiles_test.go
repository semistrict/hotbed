package worker

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// The sweep removes profile files last written before its cutoff, unless
// their command is one the engine still has; other files are left alone.
func TestRemoveStaleProfiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	files := map[string]time.Time{
		"oldgone.pb.gz":  now.Add(-31 * 24 * time.Hour),
		"oldknown.pb.gz": now.Add(-31 * 24 * time.Hour),
		"recent.pb.gz":   now.Add(-time.Hour),
		"notes.txt":      now.Add(-31 * 24 * time.Hour),
	}
	for name, at := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	if err := removeStaleProfiles(dir, now.Add(-keepProfiles), map[string]bool{"oldknown": true}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, ent := range entries {
		left = append(left, ent.Name())
	}
	if want := []string{"notes.txt", "oldknown.pb.gz", "recent.pb.gz"}; !slices.Equal(left, want) {
		t.Fatalf("left %v, want %v", left, want)
	}
}
