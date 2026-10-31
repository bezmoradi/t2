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

	keyMax          = 0x2ff
	keyBitmapBytes  = (keyMax + 1) / 8
	firstButtonCode = 0x100

	eventTypeKey = 1
)

const (
	iocDirectionRead = 2
	evdevIoctlType   = 'E'
	nrGetKeyState    = 0x18
	nrGetEventBits   = 0x20
)

func evdevReadIoctl(nr, size uintptr) uintptr {
	return iocDirectionRead<<30 | size<<16 | evdevIoctlType<<8 | nr
}

var (
	ioctlKeysDown      = evdevReadIoctl(nrGetKeyState, keyBitmapBytes)
	ioctlKeysSupported = evdevReadIoctl(nrGetEventBits+eventTypeKey, keyBitmapBytes)
)

const rescanInterval = 3 * time.Second

type keyBitmap [keyBitmapBytes]byte

func (b *keyBitmap) contains(code int) bool { return b[code/8]&(1<<(code%8)) != 0 }

func (b *keyBitmap) anyOf(codes ...int) bool {
	for _, c := range codes {
		if b.contains(c) {
			return true
		}
	}
	return false
}

func isModifierOrLock(code int) bool {
	switch code {
	case keyLeftCtrl, keyRightCtrl, keyLeftShift, keyRightShift,
		keyLeftAlt, keyRightAlt, keyLeftMeta, keyRightMeta, keyCapsLock:
		return true
	}
	return false
}

type inode uint64

type evdevKeyboard struct {
	mu                sync.Mutex
	keyboardsByPath   map[string]*os.File
	notKeyboardInodes map[string]inode
	lastScan          time.Time
}

func openEvdev() (*evdevKeyboard, error) {
	k := &evdevKeyboard{keyboardsByPath: map[string]*os.File{}, notKeyboardInodes: map[string]inode{}}
	permissionDenied := k.openNewKeyboardsLocked()
	needsInputGroup := len(k.keyboardsByPath) == 0 && permissionDenied > 0
	if needsInputGroup {
		return nil, errors.New("cannot read the keyboard: /dev/input needs the input group.\n" +
			"   On Wayland the compositor shows other programs no keys, so t2 reads the key state from the kernel.\n" +
			"   Run: sudo usermod -aG input $USER, then log out and back in.\n" +
			"   Note that every program you run can then read the keyboard, not only t2")
	}
	return k, nil
}

func (k *evdevKeyboard) source() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.keyboardsByPath) == 0 {
		return fmt.Sprintf("/dev/input, but no keyboard is connected yet: one plugged in is picked up within %v", rescanInterval)
	}
	return fmt.Sprintf("/dev/input (%d keyboard device(s))", len(k.keyboardsByPath))
}

func deviceInode(path string) inode {
	if fi, err := os.Stat(path); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			return inode(st.Ino)
		}
	}
	return 0
}

func (k *evdevKeyboard) openNewKeyboardsLocked() (permissionDenied int) {
	k.lastScan = time.Now()

	paths, _ := filepath.Glob("/dev/input/event*")
	existingPaths := map[string]bool{}
	for _, p := range paths {
		existingPaths[p] = true
		if _, open := k.keyboardsByPath[p]; open {
			continue
		}

		node := deviceInode(p)
		if known, ok := k.notKeyboardInodes[p]; ok && known == node {
			continue
		}

		f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			if errors.Is(err, fs.ErrPermission) {
				permissionDenied++
				continue
			}
			k.notKeyboardInodes[p] = node
			continue
		}
		if !isKeyboard(f) {
			f.Close()
			k.notKeyboardInodes[p] = node
			continue
		}
		k.keyboardsByPath[p] = f
	}
	for p := range k.notKeyboardInodes {
		if !existingPaths[p] {
			delete(k.notKeyboardInodes, p)
		}
	}
	return permissionDenied
}

func isKeyboard(f *os.File) bool {
	var supported keyBitmap
	if ioctl(f, ioctlKeysSupported, unsafe.Pointer(&supported[0])) != nil {
		return false
	}
	return supported.anyOf(keyLeftCtrl, keyRightCtrl) && supported.anyOf(keyLeftShift, keyRightShift)
}

func ioctl(f *os.File, req uintptr, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), req, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

func (k *evdevKeyboard) keysDownOnAnyKeyboard() keyBitmap {
	k.mu.Lock()
	defer k.mu.Unlock()

	if time.Since(k.lastScan) >= rescanInterval {
		k.openNewKeyboardsLocked()
	}

	var merged, device keyBitmap
	for p, f := range k.keyboardsByPath {
		unplugged := ioctl(f, ioctlKeysDown, unsafe.Pointer(&device[0])) != nil
		if unplugged {
			f.Close()
			delete(k.keyboardsByPath, p)
			continue
		}
		for i := range merged {
			merged[i] |= device[i]
		}
	}
	return merged
}

func (k *evdevKeyboard) hotkeyDown() bool {
	down := k.keysDownOnAnyKeyboard()
	return down.anyOf(keyLeftCtrl, keyRightCtrl) &&
		down.anyOf(keyLeftShift, keyRightShift) &&
		!down.anyOf(keyLeftAlt, keyRightAlt, keyLeftMeta, keyRightMeta)
}

func (k *evdevKeyboard) otherDown() bool {
	down := k.keysDownOnAnyKeyboard()
	for c := 1; c < firstButtonCode; c++ {
		if !isModifierOrLock(c) && down.contains(c) {
			return true
		}
	}
	return false
}
