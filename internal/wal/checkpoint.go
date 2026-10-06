package wal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Checkpoint is the acknowledged consume position. Following Prometheus
// remote write semantics, it only advances after a successful send — so any
// record at or after the checkpoint survives crashes and will be replayed.
type Checkpoint struct {
	Segment uint64 `json:"segment"`
	Offset  int64  `json:"offset"`
}

const checkpointFile = "checkpoint"

// LoadCheckpoint returns the stored checkpoint, or the zero checkpoint when
// no checkpoint file exists yet.
func LoadCheckpoint(dir string) (Checkpoint, error) {
	data, err := os.ReadFile(filepath.Join(dir, checkpointFile))
	if os.IsNotExist(err) {
		return Checkpoint{}, nil
	}
	if err != nil {
		return Checkpoint{}, err
	}
	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return Checkpoint{}, fmt.Errorf("wal: decode checkpoint: %w", err)
	}
	return cp, nil
}

// SaveCheckpoint persists the checkpoint atomically (tmp file + rename).
func SaveCheckpoint(dir string, cp Checkpoint) error {
	data, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, checkpointFile+".tmp")
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, checkpointFile))
}
