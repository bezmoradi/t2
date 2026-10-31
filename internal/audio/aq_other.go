//go:build !linux && (!darwin || !cgo)

package audio

import (
	"errors"
	"fmt"
	"runtime"
)

// OpenStream fails everywhere capture is not implemented: on every platform
// but macOS and Linux, and on macOS when the binary was built without cgo,
// which is what happens when no C compiler is installed. The stub exists so
// such a build fails with one clear sentence at the first recording rather
// than with "undefined: OpenStream" at compile time.
var OpenStream = func(sampleRate, frames int) (Stream, error) {
	if runtime.GOOS == "darwin" {
		return nil, errors.New("audio capture needs cgo: install the Xcode Command Line Tools (xcode-select --install) and reinstall t2")
	}
	return nil, fmt.Errorf("audio capture is implemented for macOS and Linux only (this is %s)", runtime.GOOS)
}
