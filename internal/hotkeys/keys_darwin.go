//go:build darwin && cgo

package hotkeys

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework CoreGraphics -framework Carbon
#include "keys_darwin.h"
*/
import "C"

func openKeyboard() error    { return nil }
func keyboardSource() string { return "" }

func ctrlShiftDown() bool {
	return int(C.onlyCtrlShiftHeld()) == 1
}

func HotkeyDown() bool { return ctrlShiftDown() }

func otherKeyDown() bool {
	return int(C.anyNonModifierKeyDown()) == 1
}
