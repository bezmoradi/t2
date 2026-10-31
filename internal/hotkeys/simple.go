//go:build darwin && cgo

package hotkeys

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework CoreGraphics -framework Carbon
#include <CoreGraphics/CoreGraphics.h>
#include <Carbon/Carbon.h>

// checkModifierKeys reports whether Ctrl+Shift is held on its own.
//
// Command and Option are rejected rather than ignored: Cmd+Ctrl+Shift+4 is
// screenshot-to-clipboard, which would otherwise start a recording and then
// race the screenshot for the pasteboard.
int checkModifierKeys() {
    CGEventFlags flags = CGEventSourceFlagsState(kCGEventSourceStateHIDSystemState);
    int ctrl  = (flags & kCGEventFlagMaskControl) != 0;
    int shift = (flags & kCGEventFlagMaskShift) != 0;
    int cmd   = (flags & kCGEventFlagMaskCommand) != 0;
    int alt   = (flags & kCGEventFlagMaskAlternate) != 0;
    return ctrl && shift && !cmd && !alt;
}

// anyNonModifierKeyDown reports whether an ordinary key is held.
//
// Dictation means "Ctrl+Shift and nothing else". Virtual keycodes 0x36-0x3F are
// the modifier keys themselves and are skipped; everything else being down means
// this is a shortcut like Ctrl+Shift+T or Ctrl+Shift+Tab, not a dictation hold.
int anyNonModifierKeyDown() {
    for (int k = 0; k < 128; k++) {
        if (k >= 0x36 && k <= 0x3F) continue;
        if (CGEventSourceKeyState(kCGEventSourceStateHIDSystemState, (CGKeyCode)k)) {
            return 1;
        }
    }
    return 0;
}
*/
import "C"

import (
	"sync"
	"time"
)

const (
	pollInterval = 100 * time.Millisecond
	// Sized so the poller never stalls during a normal press/release cycle.
	// At one edge per poll tick this holds several seconds of input.
	eventBuffer = 64
)

// edge is one transition of the hotkey state.
//
// The timestamp matters: hold duration used to be measured with time.Now()
// inside the handlers, which ran after a dial, a beep and a sleep. A genuine
// one-second hold measured as half a second and was thrown away as an
// accidental tap.
type edge struct {
	pressed bool
	at      time.Time

	// cancelled marks a release the poller produced because another key went
	// down mid-hold, with Ctrl+Shift still physically down, so the handler can
	// stop and discard the recording instead of transcribing a shortcut.
	cancelled bool

	// capped marks a release the poller produced at the hold limit, with the
	// keys still physically down.
	capped bool
}

// Manager polls the keyboard for the Ctrl+Shift hold and turns it into
// OnPress and OnRelease calls on one goroutine, in order.
type Manager struct {
	handler EventHandler

	// Both edges share one ordered channel. They used to be two separate
	// size-1 channels that dropped on overflow, which let a release edge get
	// discarded while its press survived, leaving the recorder running with no
	// key held.
	events   chan edge
	done     chan struct{}
	stopOnce sync.Once

	// The key-state probes and the hold limit are fields so the poller can be
	// driven by tests without a keyboard.
	hotkeyDown func() bool
	otherDown  func() bool
	maxHold    time.Duration
}

func NewManager(handler EventHandler) *Manager {
	return &Manager{
		handler:    handler,
		events:     make(chan edge, eventBuffer),
		done:       make(chan struct{}),
		hotkeyDown: ctrlShiftDown,
		otherDown:  otherKeyDown,
		maxHold:    MaxHold,
	}
}

func (s *Manager) Start() error {
	go s.pollKeyState()
	return nil
}

func (s *Manager) Stop() {
	s.stopOnce.Do(func() { close(s.done) })
}

func (s *Manager) Listen() {
	for {
		select {
		case e := <-s.events:
			if !e.pressed {
				// Release with no press in flight - nothing to close out.
				continue
			}

			if s.handler != nil {
				s.handler.OnPress()
			}

			release, ok := s.waitForRelease()
			if !ok {
				return
			}

			if s.handler != nil {
				s.handler.OnRelease(Release{
					Held:      release.at.Sub(e.at),
					Cancelled: release.cancelled,
					Capped:    release.capped,
				})
			}
		case <-s.done:
			return
		}
	}
}

// waitForRelease blocks until the release edge matching the press we just
// handled arrives. The bool is false if the manager was stopped first.
func (s *Manager) waitForRelease() (edge, bool) {
	for {
		select {
		case e := <-s.events:
			if !e.pressed {
				return e, true
			}
			// Duplicate press edge; keep waiting for the release.
		case <-s.done:
			return edge{}, false
		}
	}
}

func (s *Manager) pollKeyState() {
	var (
		wasPressed bool
		pressedAt  time.Time

		// ended is set once the poller has reported a release itself, with
		// the keys still down: at the hold limit, or because another key
		// went down. Until the keys physically come up the poller then
		// ignores them: it must not start another recording off a modifier
		// that is stuck down, and it must not report the eventual real
		// release twice.
		ended bool
	)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
		}

		now := time.Now()
		isPressed := s.hotkeyDown()

		switch {
		case isPressed && !wasPressed:
			// Never start a hold that already has another key down.
			if s.otherDown() {
				continue
			}
			wasPressed = true
			pressedAt = now
			if !s.emit(edge{pressed: true, at: now}) {
				return
			}

		case isPressed && wasPressed:
			if ended {
				continue
			}
			// A non-modifier key going down mid-hold means this was a
			// shortcut, not dictation. End the hold now: waiting for the keys
			// to come up kept recording, and streaming the room to the
			// server, for as long as they were held.
			if s.otherDown() {
				ended = true
				if !s.emit(edge{pressed: false, at: now, cancelled: true}) {
					return
				}
				continue
			}
			if now.Sub(pressedAt) >= s.maxHold {
				ended = true
				if !s.emit(edge{pressed: false, at: now, capped: true}) {
					return
				}
			}

		case !isPressed && wasPressed:
			wasPressed = false
			if ended {
				// Already reported.
				ended = false
				continue
			}
			if !s.emit(edge{pressed: false, at: now}) {
				return
			}
		}
	}
}

// emit queues an edge. It never drops one: if the buffer is somehow full the
// poller stalls here rather than discarding, because a lost release edge
// leaves the microphone recording with nothing to stop it. It reports false
// if the manager was stopped instead.
func (s *Manager) emit(e edge) bool {
	select {
	case s.events <- e:
		return true
	case <-s.done:
		return false
	}
}

func ctrlShiftDown() bool {
	return int(C.checkModifierKeys()) == 1
}

// HotkeyDown reports whether Ctrl+Shift is physically held right now. The
// daemon checks it before synthesising Cmd+V, which with the hotkey held would
// arrive as Ctrl+Shift+Cmd+V.
func HotkeyDown() bool { return ctrlShiftDown() }

func otherKeyDown() bool {
	return int(C.anyNonModifierKeyDown()) == 1
}
