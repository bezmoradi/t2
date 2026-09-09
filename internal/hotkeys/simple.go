//go:build darwin

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

	// cancelled marks a hold aborted because another key went down, so the
	// handler can discard the recording instead of transcribing a shortcut.
	cancelled bool
}

type SimpleHotkeyManager struct {
	handler EventHandler

	// Both edges share one ordered channel. They used to be two separate
	// size-1 channels that dropped on overflow, which let a release edge get
	// discarded while its press survived, leaving the recorder running with no
	// key held.
	events   chan edge
	done     chan struct{}
	stopOnce sync.Once
}

func NewSimpleManager(handler EventHandler) *SimpleHotkeyManager {
	return &SimpleHotkeyManager{
		handler: handler,
		events:  make(chan edge, eventBuffer),
		done:    make(chan struct{}),
	}
}

func (s *SimpleHotkeyManager) Start() error {
	go s.pollKeyState()
	return nil
}

func (s *SimpleHotkeyManager) Stop() {
	s.stopOnce.Do(func() { close(s.done) })
}

func (s *SimpleHotkeyManager) Listen() {
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
				s.handler.OnRelease(release.at.Sub(e.at), release.cancelled)
			}
		case <-s.done:
			return
		}
	}
}

// waitForRelease blocks until the release edge matching the press we just
// handled arrives. The bool is false if the manager was stopped first.
func (s *SimpleHotkeyManager) waitForRelease() (edge, bool) {
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

func (s *SimpleHotkeyManager) pollKeyState() {
	wasPressed := false
	cancelled := false

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
		}

		now := time.Now()
		isPressed := s.detectCtrlShift()

		// A non-modifier key going down mid-hold means this was a shortcut, not
		// dictation. Latch it so the release reports the hold as cancelled.
		if isPressed && wasPressed && !cancelled && s.otherKeyDown() {
			cancelled = true
		}

		// Never start a hold that already has another key down.
		if isPressed && !wasPressed && s.otherKeyDown() {
			continue
		}

		if isPressed == wasPressed {
			continue
		}
		wasPressed = isPressed

		e := edge{pressed: isPressed, at: now}
		if !isPressed {
			e.cancelled = cancelled
			cancelled = false
		}

		// Never drop an edge. If the buffer is somehow full the poller stalls
		// here rather than discarding, because a lost release edge leaves the
		// microphone recording with nothing to stop it.
		select {
		case s.events <- e:
		case <-s.done:
			return
		}
	}
}

func (s *SimpleHotkeyManager) detectCtrlShift() bool {
	return int(C.checkModifierKeys()) == 1
}

func (s *SimpleHotkeyManager) otherKeyDown() bool {
	return int(C.anyNonModifierKeyDown()) == 1
}
