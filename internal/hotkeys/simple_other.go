//go:build !linux && (!darwin || !cgo)

package hotkeys

import (
	"errors"
	"fmt"
	"runtime"
)

type Manager struct{ handler EventHandler }

func NewManager(handler EventHandler) *Manager {
	return &Manager{handler: handler}
}

func (s *Manager) Start() error {
	if runtime.GOOS == "darwin" {
		return errors.New("hotkey detection needs cgo: install the Xcode Command Line Tools (xcode-select --install) and reinstall t2")
	}
	return fmt.Errorf("hotkey detection is implemented for macOS and Linux only (this is %s)", runtime.GOOS)
}

func (s *Manager) Stop()          {}
func (s *Manager) Listen()        {}
func (s *Manager) Source() string { return "" }

func HotkeyDown() bool { return false }
