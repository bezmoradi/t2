package hotkeys

import (
	"fmt"
	"os"
	"sync/atomic"
)

// keyboard is one way of reading the physical key state on Linux. Both
// implementations answer the same two questions the macOS poller asks
// CoreGraphics, from a snapshot of every key that is down, so a hold behaves
// the same on every platform: Ctrl+Shift alone starts it, Alt or Super with
// it is rejected, and any other key cancels it.
type keyboard interface {
	hotkeyDown() bool
	otherDown() bool
	source() string
}

// active is the keyboard Start opened. HotkeyDown reads it from the daemon's
// goroutines while the poller reads it from its own.
var active atomic.Pointer[keyboard]

// openKeyboard picks how to read the keys.
//
// On X11 the X server reports the key state to any client, so no permission
// is needed. A Wayland compositor shows a client only the keys typed into its
// own windows - X11 clients under XWayland included - so a global hold can
// only be seen in the kernel's input devices, which belong to the input group.
func openKeyboard() error {
	var kb keyboard
	if os.Getenv("WAYLAND_DISPLAY") == "" && os.Getenv("DISPLAY") != "" {
		x, err := openX11()
		if err != nil {
			e, evErr := openEvdev()
			if evErr != nil {
				return fmt.Errorf("%v; %v", err, evErr)
			}
			kb = e
		} else {
			kb = x
		}
	} else {
		e, err := openEvdev()
		if err != nil {
			return err
		}
		kb = e
	}
	active.Store(&kb)
	return nil
}

func current() keyboard {
	if kb := active.Load(); kb != nil {
		return *kb
	}
	return nil
}

func keyboardSource() string {
	if kb := current(); kb != nil {
		return kb.source()
	}
	return ""
}

func ctrlShiftDown() bool {
	kb := current()
	return kb != nil && kb.hotkeyDown()
}

// HotkeyDown reports whether Ctrl+Shift is physically held right now. The
// daemon checks it before synthesising Ctrl+V, which with the hotkey held
// would arrive as Ctrl+Shift+V.
func HotkeyDown() bool { return ctrlShiftDown() }

func otherKeyDown() bool {
	kb := current()
	return kb != nil && kb.otherDown()
}
