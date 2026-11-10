package app

import (
	"crime-buster/internal/config"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func CheckHealthFile(dataDir string, now time.Time) error {
	fi, err := os.Stat(filepath.Join(dataDir, "health"))
	if err != nil {
		return err
	}
	if now.Sub(fi.ModTime()) > 2*time.Minute {
		return fmt.Errorf("health file is stale: %s", fi.ModTime())
	}
	return nil
}

func TouchHealth(cfg *config.Config) {
	_ = os.MkdirAll(cfg.StorageConfig.DataDir, 0o755)
	_ = os.WriteFile(filepath.Join(cfg.StorageConfig.DataDir, "health"), []byte(time.Now().Format(time.RFC3339)), 0o644)
}
