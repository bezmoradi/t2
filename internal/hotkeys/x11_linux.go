package hotkeys

import (
	"fmt"
	"io"
	"log"
	"sync"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

const (
	modifierRowShift   = 0
	modifierRowControl = 2
	modifierRowMod1    = 3
	modifierRowMod5    = 7

	x11FirstKeycode = 8
	x11KeycodeCount = 256
	x11KeymapBytes  = x11KeycodeCount / 8
)

type x11Keyboard struct {
	conn *xgb.Conn

	ctrlKeycodes       []byte
	shiftKeycodes      []byte
	altOrSuperKeycodes []byte
	isModifierKeycode  [x11KeycodeCount]bool

	reportLostConnection sync.Once
}

func openX11() (*x11Keyboard, error) {
	xgb.Logger = log.New(io.Discard, "", 0)

	conn, err := xgb.NewConn()
	if err != nil {
		return nil, fmt.Errorf("cannot connect to the X server: %v", err)
	}

	mm, err := xproto.GetModifierMapping(conn).Reply()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("reading the X modifier map: %v", err)
	}
	keycodesPerRow := int(mm.KeycodesPerModifier)
	keycodesInRow := func(row int) []byte {
		var out []byte
		for _, kc := range mm.Keycodes[row*keycodesPerRow : (row+1)*keycodesPerRow] {
			if kc != 0 {
				out = append(out, byte(kc))
			}
		}
		return out
	}

	k := &x11Keyboard{
		conn:          conn,
		shiftKeycodes: keycodesInRow(modifierRowShift),
		ctrlKeycodes:  keycodesInRow(modifierRowControl),
	}
	for row := modifierRowMod1; row <= modifierRowMod5; row++ {
		k.altOrSuperKeycodes = append(k.altOrSuperKeycodes, keycodesInRow(row)...)
	}
	for _, kc := range mm.Keycodes {
		k.isModifierKeycode[kc] = true
	}
	if len(k.ctrlKeycodes) == 0 || len(k.shiftKeycodes) == 0 {
		conn.Close()
		return nil, fmt.Errorf("the X keyboard map has no Control or Shift key")
	}
	return k, nil
}

func (k *x11Keyboard) source() string { return "the X server (X11)" }

func (k *x11Keyboard) keysDown() ([]byte, bool) {
	r, err := xproto.QueryKeymap(k.conn).Reply()
	if err != nil || len(r.Keys) < x11KeymapBytes {
		k.reportLostConnection.Do(func() { log.Printf("hotkey: lost the X server connection: %v", err) })
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
	keys, ok := k.keysDown()
	return ok && x11AnyDown(keys, k.ctrlKeycodes) && x11AnyDown(keys, k.shiftKeycodes) && !x11AnyDown(keys, k.altOrSuperKeycodes)
}

func (k *x11Keyboard) otherDown() bool {
	keys, ok := k.keysDown()
	if !ok {
		return false
	}
	for kc := x11FirstKeycode; kc < x11KeycodeCount; kc++ {
		if !k.isModifierKeycode[kc] && x11Down(keys, byte(kc)) {
			return true
		}
	}
	return false
}
