package hotkeys

import (
	"fmt"
	"io"
	"log"
	"sync"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

// x11Keyboard polls the X server's key map (QueryKeymap), the X11 counterpart
// of CGEventSourceKeyState. It speaks the X protocol in Go, so the build needs
// neither cgo nor libX11's headers.
type x11Keyboard struct {
	conn *xgb.Conn

	ctrl, shift []byte // keycodes on the Control and Shift modifier rows
	reject      []byte // keycodes on Mod1-Mod5: Alt, AltGr, Super, Meta
	modifier    [256]bool

	lost sync.Once
}

func openX11() (*x11Keyboard, error) {
	// xgb logs to stderr on its own, including a harmless note on every
	// connection made without an ~/.Xauthority file.
	xgb.Logger = log.New(io.Discard, "", 0)

	conn, err := xgb.NewConn()
	if err != nil {
		return nil, fmt.Errorf("cannot connect to the X server: %v", err)
	}

	// The modifier map says which keycodes act as which modifier, so a
	// remapped keyboard (Caps Lock as Control, say) is handled as the user
	// set it up. Rows: 0 Shift, 1 Lock, 2 Control, 3-7 Mod1-Mod5.
	mm, err := xproto.GetModifierMapping(conn).Reply()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("reading the X modifier map: %v", err)
	}
	per := int(mm.KeycodesPerModifier)
	row := func(i int) []byte {
		var out []byte
		for _, kc := range mm.Keycodes[i*per : (i+1)*per] {
			if kc != 0 {
				out = append(out, byte(kc))
			}
		}
		return out
	}

	k := &x11Keyboard{conn: conn, shift: row(0), ctrl: row(2)}
	for i := 3; i < 8; i++ {
		k.reject = append(k.reject, row(i)...)
	}
	for _, kc := range mm.Keycodes {
		k.modifier[kc] = true
	}
	if len(k.ctrl) == 0 || len(k.shift) == 0 {
		conn.Close()
		return nil, fmt.Errorf("the X keyboard map has no Control or Shift key")
	}
	return k, nil
}

func (k *x11Keyboard) source() string { return "the X server (X11)" }

// keys returns the bitmap of keys down, one bit per keycode.
func (k *x11Keyboard) keys() ([]byte, bool) {
	r, err := xproto.QueryKeymap(k.conn).Reply()
	if err != nil || len(r.Keys) < 32 {
		k.lost.Do(func() { log.Printf("hotkey: lost the X server connection: %v", err) })
		return nil, false
	}
	return r.Keys, true
}

func x11Down(keys []byte, kc byte) bool { return keys[kc/8]&(1<<(kc%8)) != 0 }

func x11AnyDown(keys []byte, set []byte) bool {
	for _, kc := range set {
		if x11Down(keys, kc) {
			return true
		}
	}
	return false
}

func (k *x11Keyboard) hotkeyDown() bool {
	keys, ok := k.keys()
	return ok && x11AnyDown(keys, k.ctrl) && x11AnyDown(keys, k.shift) && !x11AnyDown(keys, k.reject)
}

func (k *x11Keyboard) otherDown() bool {
	keys, ok := k.keys()
	if !ok {
		return false
	}
	for kc := 8; kc < 256; kc++ {
		if !k.modifier[kc] && x11Down(keys, byte(kc)) {
			return true
		}
	}
	return false
}
