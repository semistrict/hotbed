package t3bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/semistrict/hotbed/internal/setup"
)

const (
	stateFileVersion     = 1
	maxPersistedNotified = 10000
)

type persistedState struct {
	Version  int              `json:"version"`
	Notified map[string]int64 `json:"notified"`
}

// StateStore persists notified (questionID -> revision) entries on disk so
// bridge restarts and WebSocket reconnects never double-notify.
type StateStore struct {
	Path string
}

// DefaultStateFile returns ~/.hotbed/t3-bridge-state.json.
func DefaultStateFile() (string, error) {
	dir, err := setup.DefaultDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "t3-bridge-state.json"), nil
}

// Load reads the persisted questionID -> revision map, returning an empty map
// when the state file does not exist yet.
func (s *StateStore) Load() (map[int32]int64, error) {
	out := map[int32]int64{}
	if s == nil || s.Path == "" {
		return out, nil
	}
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read t3-bridge state %s: %w", s.Path, err)
	}
	var doc persistedState
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse t3-bridge state %s: %w", s.Path, err)
	}
	for k, rev := range doc.Notified {
		id, err := strconv.ParseInt(k, 10, 32)
		if err != nil || id <= 0 || rev <= 0 {
			continue
		}
		out[int32(id)] = rev
	}
	return out, nil
}

// Save atomically persists notified (questionID -> revision) to disk.
func (s *StateStore) Save(notified map[int32]int64) error {
	if s == nil || s.Path == "" {
		return nil
	}
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create t3-bridge state dir %s: %w", dir, err)
	}

	ids := make([]int32, 0, len(notified))
	for id, rev := range notified {
		if id > 0 && rev > 0 {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	if len(ids) > maxPersistedNotified {
		ids = ids[len(ids)-maxPersistedNotified:]
	}

	doc := persistedState{
		Version:  stateFileVersion,
		Notified: make(map[string]int64, len(ids)),
	}
	for _, id := range ids {
		doc.Notified[strconv.Itoa(int(id))] = notified[id]
	}

	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')

	tmp, err := os.CreateTemp(dir, "t3-bridge-state.*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.Path); err != nil {
		return fmt.Errorf("replace t3-bridge state %s: %w", s.Path, err)
	}
	return nil
}
