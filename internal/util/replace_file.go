package util

import (
	"bytes"
	"os"
	"path/filepath"
)

// ReplaceFile atomically writes data to path, leaving the file alone when it already holds
// exactly data, so a state file checkpointed often costs a disk write, and a sync to the
// other machines, only when it changes.
func ReplaceFile(path string, data []byte) error {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
