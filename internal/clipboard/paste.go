package clipboard

import (
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// CopyText puts text on the clipboard without pasting it.
func CopyText(text string) error {
	if text == "" {
		return fmt.Errorf("empty text")
	}

	cmd := exec.Command("pbcopy")
	cmd.Stdin = strings.NewReader(text)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to copy to clipboard: %v", err)
	}

	return nil
}

// PasteTextSafely copies text to clipboard and pastes it using AppleScript
func PasteTextSafely(text string) error {
	if err := CopyText(text); err != nil {
		return err
	}

	// Small delay to ensure clipboard is set
	time.Sleep(200 * time.Millisecond)

	// Send Cmd+V exactly once.
	//
	// This is deliberately not retried. osascript can report a failure - an
	// AppleEvent timeout being the common one - after the keystroke has already
	// been delivered, so retrying pastes the text a second time. The text is on
	// the clipboard either way: a missed paste is recoverable with Cmd+V, while
	// a duplicated one silently corrupts whatever is being typed into.
	script := `tell application "System Events" to keystroke "v" using command down`
	output, err := exec.Command("osascript", "-e", script).CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if detail != "" {
			detail = ": " + detail
		}
		return fmt.Errorf("paste keystroke failed (text is on the clipboard, press Cmd+V): %v%s", err, detail)
	}

	return nil
}
