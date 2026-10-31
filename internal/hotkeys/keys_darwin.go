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

// CoreGraphics needs nothing opened: the HID state is always there to read.
func openKeyboard() error    { return nil }
func keyboardSource() string { return "" }

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
