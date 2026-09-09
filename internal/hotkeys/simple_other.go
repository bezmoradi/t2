//go:build !darwin

package hotkeys

import (
	"fmt"
	"runtime"
)

// Hotkey detection is CoreGraphics-specific. This stub exists so a non-macOS
// build fails with one clear sentence at startup rather than a page of cgo
// errors at compile time - or, worse, a binary that starts, prints its banner,
// and silently never records.
type SimpleHotkeyManager struct{ handler EventHandler }

func NewSimpleManager(handler EventHandler) *SimpleHotkeyManager {
	return &SimpleHotkeyManager{handler: handler}
}

func (s *SimpleHotkeyManager) Start() error {
	return fmt.Errorf("hotkey detection is implemented for macOS only (this is %s)", runtime.GOOS)
}

func (s *SimpleHotkeyManager) Stop()   {}
func (s *SimpleHotkeyManager) Listen() {}
