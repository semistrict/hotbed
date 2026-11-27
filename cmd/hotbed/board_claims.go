package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/semistrict/hotbed/internal/api"
)

// boardClaimRecord persists the single-issued fenced claim proof (epoch + token)
// on disk with 0600 permissions so subsequent renew/release/complete/fail/block
// commands can prove ownership without exposing the token on stdout.
type boardClaimRecord struct {
	TaskID int64  `json:"taskId"`
	Epoch  int32  `json:"epoch"`
	Token  string `json:"token"`
	Until  int64  `json:"until"`
}

func boardClaimsDir(d deps) string {
	base := d.cache
	if base == "" {
		base = cacheDir()
	}
	return filepath.Join(base, "board-claims")
}

func boardClaimPath(d deps, controller string, taskID int64, sessionID, asProfile string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s\x00%s", strings.TrimRight(controller, "/"), taskID, sessionID, asProfile)))
	return filepath.Join(boardClaimsDir(d), fmt.Sprintf("task-%d-%s.json", taskID, hex.EncodeToString(sum[:8])))
}

func saveBoardClaim(d deps, controller string, taskID int64, sessionID, asProfile string, rec boardClaimRecord) error {
	dir := boardClaimsDir(d)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	path := boardClaimPath(d, controller, taskID, sessionID, asProfile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func loadBoardClaim(d deps, controller string, taskID int64, sessionID, asProfile string) (boardClaimRecord, bool, error) {
	path := boardClaimPath(d, controller, taskID, sessionID, asProfile)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return boardClaimRecord{}, false, nil
		}
		return boardClaimRecord{}, false, err
	}
	var rec boardClaimRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return boardClaimRecord{}, false, err
	}
	return rec, rec.Token != "", nil
}

func deleteBoardClaim(d deps, controller string, taskID int64, sessionID, asProfile string) {
	_ = os.Remove(boardClaimPath(d, controller, taskID, sessionID, asProfile))
}

func resolveBoardClaimFence(d deps, controller string, taskID int64, sessionID, asProfile string, explicitEpoch int32, explicitToken string) (int32, string, error) {
	if explicitToken != "" {
		return explicitEpoch, explicitToken, nil
	}
	if env := strings.TrimSpace(os.Getenv("HOTBED_BOARD_TASK")); env != "" {
		var envTask struct {
			ID    int64  `json:"id"`
			Epoch int32  `json:"epoch"`
			Token string `json:"token"`
		}
		if json.Unmarshal([]byte(env), &envTask) == nil && envTask.ID == taskID && envTask.Token != "" {
			epoch := explicitEpoch
			if epoch == 0 {
				epoch = envTask.Epoch
			}
			return epoch, envTask.Token, nil
		}
	}
	rec, ok, err := loadBoardClaim(d, controller, taskID, sessionID, asProfile)
	if err != nil {
		return 0, "", err
	}
	if !ok {
		return 0, "", api.Errorf(api.CodeConflict, "no local claim token stored for task #%d", taskID).
			WithSuggestion(fmt.Sprintf("pass --token and --epoch, or claim #%d first with `hotbed board task claim %d`", taskID, taskID))
	}
	epoch := explicitEpoch
	if epoch == 0 {
		epoch = rec.Epoch
	}
	return epoch, rec.Token, nil
}
