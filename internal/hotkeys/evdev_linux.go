package hotkeys

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Key codes from linux/input-event-codes.h.
const (
	keyLeftCtrl   = 29
	keyLeftShift  = 42
	keyRightShift = 54
	keyLeftAlt    = 56
	keyCapsLock   = 58
	keyRightCtrl  = 97
	keyRightAlt   = 100
	keyLeftMeta   = 125
	keyRightMeta  = 126

	// keyMax+1 bits is the size of the kernel's key-state bitmap. Codes
	// from 0x100 up are buttons (mouse, joystick, stylus), not keys.
	keyMax      = 0x2ff
	keyBytes    = (keyMax + 1) / 8
	lastKeyCode = 0xff

	evKey = 1
)

// The ioctls are encoded as the generic _IOR('E', nr, len) of
// asm-generic/ioctl.h, which amd64, arm64, 386, arm and riscv64 share.
func evdevIOR(nr, size uintptr) uintptr { return 2<<30 | size<<16 | 'E'<<8 | nr }

var (
	eviocgkey      = evdevIOR(0x18, keyBytes)       // EVIOCGKEY: keys down now
	eviocgbitEvKey = evdevIOR(0x20+evKey, keyBytes) // EVIOCGBIT(EV_KEY): keys it has
)

// rescanInterval is how often the device list is refreshed, so a keyboard
// plugged in after startup is picked up.
const rescanInterval = 3 * time.Second

// evdevKeyboard reads key state straight from the kernel's input devices.
//
// It never reads events. EVIOCGKEY returns the bitmap of keys down at that
// moment, the evdev counterpart of CGEventSourceKeyState, so the poller is
// the same stateless one as on macOS: nothing to fall behind on, and no
// stream of keystrokes ever passes through t2.
type evdevKeyboard struct {
	mu       sync.Mutex
	devices  map[string]*os.File // open keyboards by path
	skip     map[string]uint64   // path -> inode of devices that are not keyboards
	lastScan time.Time
}

func openEvdev() (*evdevKeyboard, error) {
	k := &evdevKeyboard{devices: map[string]*os.File{}, skip: map[string]uint64{}}
	denied := k.scan()
	if len(k.devices) > 0 {
		return k, nil
	}
	if denied > 0 {
		return nil, errors.New("cannot read the keyboard: /dev/input needs the input group.\n" +
			"   On Wayland the compositor shows other programs no keys, so t2 reads the key state from the kernel.\n" +
			"   Run: sudo usermod -aG input $USER, then log out and back in.\n" +
			"   Note that every program you run can then read the keyboard, not only t2")
	}
	return nil, errors.New("no keyboard found under /dev/input")
}

func (k *evdevKeyboard) source() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return fmt.Sprintf("/dev/input (%d keyboard device(s))", len(k.devices))
}

// scan opens keyboards that have appeared and reports how many devices
// could not be opened for lack of permission. Called with mu held, or before
// the keyboard is shared.
func (k *evdevKeyboard) scan() (denied int) {
	k.lastScan = time.Now()

	paths, _ := filepath.Glob("/dev/input/event*")
	present := map[string]bool{}
	for _, p := range paths {
		present[p] = true
		if _, open := k.devices[p]; open {
			continue
		}

		// A path is only skipped while it is the same device node: a
		// keyboard plugged in later can be given a number a mouse had.
		var ino uint64
		if fi, err := os.Stat(p); err == nil {
			if st, ok := fi.Sys().(*syscall.Stat_t); ok {
				ino = st.Ino
			}
		}
		if seen, ok := k.skip[p]; ok && seen == ino {
			continue
		}

		f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			if errors.Is(err, fs.ErrPermission) {
				denied++
			}
			k.skip[p] = ino
			continue
		}
		if !isKeyboard(f) {
			f.Close()
			k.skip[p] = ino
			continue
		}
		k.devices[p] = f
	}
	for p := range k.skip {
		if !present[p] {
			delete(k.skip, p)
		}
	}
	return denied
}

// isKeyboard reports whether a device has both Control and Shift keys, which
// rules out mice, power buttons, lid switches and the like.
func isKeyboard(f *os.File) bool {
	var bits [keyBytes]byte
	if ioctl(f, eviocgbitEvKey, unsafe.Pointer(&bits[0])) != nil {
		return false
	}
	has := func(c int) bool { return bits[c/8]&(1<<(c%8)) != 0 }
	return (has(keyLeftCtrl) || has(keyRightCtrl)) && (has(keyLeftShift) || has(keyRightShift))
}

func ioctl(f *os.File, req uintptr, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), req, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

// keys returns every key down on any keyboard, merged.
func (k *evdevKeyboard) keys() [keyBytes]byte {
	k.mu.Lock()
	defer k.mu.Unlock()

	if time.Since(k.lastScan) >= rescanInterval {
		k.scan()
	}

	var all, one [keyBytes]byte
	for p, f := range k.devices {
		if err := ioctl(f, eviocgkey, unsafe.Pointer(&one[0])); err != nil {
			// Unplugged (ENODEV), most likely; the next scan finds it again
			// if it comes back.
			f.Close()
			delete(k.devices, p)
			continue
		}
		for i := range all {
			all[i] |= one[i]
		}
	}
	return all
}

func evDown(keys *[keyBytes]byte, c int) bool { return keys[c/8]&(1<<(c%8)) != 0 }

func (k *evdevKeyboard) hotkeyDown() bool {
	keys := k.keys()
	ctrl := evDown(&keys, keyLeftCtrl) || evDown(&keys, keyRightCtrl)
	shift := evDown(&keys, keyLeftShift) || evDown(&keys, keyRightShift)
	reject := evDown(&keys, keyLeftAlt) || evDown(&keys, keyRightAlt) ||
		evDown(&keys, keyLeftMeta) || evDown(&keys, keyRightMeta)
	return ctrl && shift && !reject
}

func (k *evdevKeyboard) otherDown() bool {
	keys := k.keys()
	for c := 1; c <= lastKeyCode; c++ {
		switch c {
		case keyLeftCtrl, keyRightCtrl, keyLeftShift, keyRightShift,
			keyLeftAlt, keyRightAlt, keyLeftMeta, keyRightMeta, keyCapsLock:
			continue
		}
		if evDown(&keys, c) {
			return true
		}
	}
	return false
}
