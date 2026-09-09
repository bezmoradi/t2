package hotkeys

import "time"

type EventHandler interface {
	OnPress()
	// OnRelease reports how long the key was actually held, measured from the
	// poller's own edge timestamps rather than from wall-clock time inside the
	// handler, and whether the hold was cancelled by another key going down.
	OnRelease(held time.Duration, cancelled bool)
}

type Manager struct {
	simple *SimpleHotkeyManager
}

func NewManager(handler EventHandler) *Manager {
	return &Manager{
		simple: NewSimpleManager(handler),
	}
}

func (m *Manager) Start() error {
	return m.simple.Start()
}

func (m *Manager) Stop() {
	m.simple.Stop()
}

func (m *Manager) Listen() {
	m.simple.Listen()
}

func (m *Manager) GetHotkeyDisplay() string {
	return "Ctrl+Shift"
}
