package hotkeys

type EventHandler interface {
	OnPress()
	OnRelease()
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

func (m *Manager) UpdateConfig() error {
	// No config needed - hotkey is hardcoded
	return nil
}

func (m *Manager) GetHotkeyDisplay() string {
	return "Ctrl+Shift"
}

func (m *Manager) GetEngineType() string {
	return "simple"
}

func (m *Manager) IsUsingPrimaryEngine() bool {
	return true
}
