//go:build !linux && (!darwin || !cgo)

package hotkeys

import (
	"errors"
	"fmt"
	"runtime"
)

// Hotkey detection exists for macOS (CoreGraphics, through cgo) and Linux.
// This stub exists so a build without it - another platform, or macOS
// without cgo, which is what a missing C compiler produces - fails with one
// clear sentence at startup rather than a page of "undefined:" errors at
// compile time, or, worse, a binary that starts, prints its banner, and
// silently never records.
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

// HotkeyDown always reports false: nothing here can see the keyboard.
func HotkeyDown() bool { return false }
