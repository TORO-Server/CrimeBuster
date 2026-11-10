package store

import (
	"crime-buster/internal/domain"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func RecordPath(dir string) string { return filepath.Join(dir, "record.json") }

func LoadRecord(dir string) (*domain.Record, error) {
	b, err := os.ReadFile(RecordPath(dir))
	if err != nil {
		return nil, err
	}
	var r domain.Record
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", RecordPath(dir), err)
	}
	return &r, nil
}

// saveRecord は一時ファイルに書いてから置き換える。
func SaveRecord(dir string, r *domain.Record) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := RecordPath(dir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, RecordPath(dir))
}

// ErrNoRecord は、その ID の事件が無いとき。
var ErrNoRecord = errors.New("incident not found")
