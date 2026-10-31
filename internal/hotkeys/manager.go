package hotkeys

import "time"

const Display = "Ctrl+Shift"

const MaxHold = 5 * time.Minute

type Release struct {
	HeldFor             time.Duration
	CancelledByOtherKey bool
	CappedAtMaxHold     bool
}

type EventHandler interface {
	OnPress()
	OnRelease(Release)
}
