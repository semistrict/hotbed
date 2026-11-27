package storage

import (
	"errors"
	"path/filepath"
	"testing"
)

// NewDir works on any filesystem, on every platform: it makes no volume,
// and measures the space of the filesystem the state directory is on.
func TestNewDirKeepsNoVolumes(t *testing.T) {
	dir := t.TempDir()
	s := NewDir(dir)
	if got := s.Kind(); got != "none" {
		t.Fatalf("Kind() = %q, want none", got)
	}
	if got, want := s.Path("ws/a"), filepath.Join(dir, "volumes", "ws", "a"); got != want {
		t.Fatalf("Path(ws/a) = %q, want %q", got, want)
	}
	if err := s.Create(t.Context(), "ws/a"); !errors.Is(err, ErrNoVolumes) {
		t.Fatalf("Create = %v, want ErrNoVolumes", err)
	}
	if err := s.Clone(t.Context(), "snap", "ws/a"); !errors.Is(err, ErrNoVolumes) {
		t.Fatalf("Clone = %v, want ErrNoVolumes", err)
	}
	vols, err := s.ListVolumes(t.Context())
	if err != nil || vols != nil {
		t.Fatalf("ListVolumes = %v, %v; want nil, nil", vols, err)
	}
	sp, err := s.Space(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if sp.Size <= 0 || sp.Free <= 0 || sp.Free > sp.Size {
		t.Fatalf("Space = %+v, want 0 < Free <= Size", sp)
	}
}
