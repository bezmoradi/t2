//go:build !darwin

package audio

import (
	"fmt"
	"runtime"
)

// OpenStream fails on every platform but macOS, where capture is implemented
// with AudioQueue. The stub exists so a non-macOS build fails with one clear
// sentence at the first recording rather than at compile time.
var OpenStream = func(sampleRate, frames int) (Stream, error) {
	return nil, fmt.Errorf("audio capture is implemented for macOS only (this is %s)", runtime.GOOS)
}
