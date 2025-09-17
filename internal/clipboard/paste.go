package clipboard

import (
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// PasteTextSafely copies text to clipboard and pastes it using AppleScript
func PasteTextSafely(text string) error {
	if text == "" {
		return fmt.Errorf("empty text")
	}

	// Copy text to clipboard using macOS pbcopy command
	cmd := exec.Command("pbcopy")
	cmd.Stdin = strings.NewReader(text)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to copy to clipboard: %v", err)
	}

	// Small delay to ensure clipboard is set
	time.Sleep(200 * time.Millisecond)

	// Try different paste methods for better compatibility
	// Method 1: Try simple AppleScript paste first
	script := `tell application "System Events" to keystroke "v" using command down`
	cmd = exec.Command("osascript", "-e", script)
	if err := cmd.Run(); err != nil {
		// Method 2: If that fails, try with longer delay and error handling
		time.Sleep(500 * time.Millisecond)

		// More robust AppleScript with error handling
		script = `
		try
			tell application "System Events"
				keystroke "v" using command down
			end tell
		on error errorMessage
			return "Error: " & errorMessage
		end try`

		cmd = exec.Command("osascript", "-e", script)
		output, err2 := cmd.CombinedOutput()
		if err2 != nil {
			return fmt.Errorf("paste failed - Method 1: %v, Method 2: %v, Output: %s", err, err2, string(output))
		}
	}

	return nil
}
