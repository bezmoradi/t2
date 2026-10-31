//go:build !linux && (!darwin || !cgo)

package audio

import (
	"errors"
	"fmt"
	"runtime"
)

func OpenStream(sampleRate, frames int) (Stream, error) {
	if runtime.GOOS == "darwin" {
		return nil, errors.New("audio capture needs cgo: install the Xcode Command Line Tools (xcode-select --install) and reinstall t2")
	}
	return nil, fmt.Errorf("audio capture is implemented for macOS and Linux only (this is %s)", runtime.GOOS)
}
