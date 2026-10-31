package hotkeys

import (
	"fmt"
	"os"
	"sync/atomic"
)

type keyboard interface {
	hotkeyDown() bool
	otherDown() bool
	source() string
}

var openedKeyboard atomic.Pointer[keyboard]

func openKeyboard() error {
	x11Session := os.Getenv("WAYLAND_DISPLAY") == "" && os.Getenv("DISPLAY") != ""
	var kb keyboard
	if x11Session {
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
	openedKeyboard.Store(&kb)
	return nil
}

func currentKeyboard() keyboard {
	if kb := openedKeyboard.Load(); kb != nil {
		return *kb
	}
	return nil
}

func keyboardSource() string {
	if kb := currentKeyboard(); kb != nil {
		return kb.source()
	}
	return ""
}

func ctrlShiftDown() bool {
	kb := currentKeyboard()
	return kb != nil && kb.hotkeyDown()
}

func HotkeyDown() bool { return ctrlShiftDown() }

func otherKeyDown() bool {
	kb := currentKeyboard()
	return kb != nil && kb.otherDown()
}
