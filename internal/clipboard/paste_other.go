//go:build !darwin && !linux

package clipboard

import (
	"fmt"
	"runtime"
)

const PasteKeys = "Ctrl+V"

func Limitation() string {
	return fmt.Sprintf("the clipboard is implemented for macOS and Linux only (this is %s)", runtime.GOOS)
}

func CopyText(text string) error { return fmt.Errorf("failed to copy to clipboard: %s", Limitation()) }

func PasteOnce(text string) error { return CopyText(text) }
