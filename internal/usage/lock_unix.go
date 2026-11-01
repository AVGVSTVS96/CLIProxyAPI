//go:build unix

package usage

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// lockState keeps one process at a time writing a state directory.
func lockState(dir string) (func(), error) {
	file, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, errors.New("another process is using " + dir)
	}
	return func() { _ = file.Close() }, nil
}
