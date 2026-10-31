//go:build unix

package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/bezmoradi/t2/internal/config"
)

const instanceLockName = "t2.lock"

var errAlreadyRunning = errors.New("t2 is already running, in another terminal or tab - two would both transcribe and paste every dictation")

func LockInstance() (heldUntilClosed *os.File, err error) {
	dir, err := config.EnsureConfigDir()
	if err != nil {
		return nil, err
	}
	return flockCloseOnExec(filepath.Join(dir, instanceLockName))
}

func flockCloseOnExec(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errAlreadyRunning
		}
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	return f, nil
}
