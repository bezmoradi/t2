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

// errAlreadyRunning means another t2 daemon holds the instance lock.
var errAlreadyRunning = errors.New("t2 is already running, in another terminal or tab - two would both transcribe and paste every dictation")

// LockInstance makes sure only one t2 daemon runs per user. The returned file
// holds the lock for as long as it stays open; keep it until exit.
//
// The lock is an flock on ~/.config/t2/t2.lock. Go opens files close-on-exec,
// so the automatic restart releases it at exec and the new image takes it
// again.
func LockInstance() (*os.File, error) {
	dir, err := config.EnsureConfigDir()
	if err != nil {
		return nil, err
	}
	return lockFile(filepath.Join(dir, "t2.lock"))
}

func lockFile(path string) (*os.File, error) {
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
