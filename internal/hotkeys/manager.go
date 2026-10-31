package hotkeys

import "time"

// Display is the hotkey as shown to the user.
const Display = "Ctrl+Shift"

// MaxHold is the longest a single hold may run. Past it the poller reports a
// release while the keys are still down, then ignores them until they are
// physically let go.
//
// Without a limit, a hold whose release never arrived - a modifier the HID
// system believes is still down, which remote-desktop tools, VMs and flaky
// keyboards all produce - kept the microphone open indefinitely, grew the
// in-memory recording by ~115MB an hour, kept the session billed, and left
// the daemon deaf to every later press until restart. Five minutes is longer
// than any dictation and short enough that none of that matters.
const MaxHold = 5 * time.Minute

// Release describes how a hold ended.
type Release struct {
	// Held is how long the keys were actually down, measured from the poller's
	// own edge timestamps rather than from wall-clock time inside the handler.
	Held time.Duration

	// Cancelled means another key went down during the hold - a shortcut such
	// as Ctrl+Shift+T, not dictation. It is reported the moment that key goes
	// down, with Ctrl+Shift still held, so the recording stops at once instead
	// of streaming until the keys come up.
	Cancelled bool

	// Capped means the hold reached MaxHold and the poller ended it while the
	// keys were still down.
	Capped bool
}

type EventHandler interface {
	OnPress()
	OnRelease(Release)
}
