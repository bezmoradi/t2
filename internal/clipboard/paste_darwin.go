package clipboard

import (
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const PasteKeys = "Cmd+V"

const (
	clipboardSettleDelay = 200 * time.Millisecond
	cmdVKeystrokeScript  = `tell application "System Events" to keystroke "v" using command down`
)

func Limitation() string { return "" }

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

func PasteOnce(text string) error {
	if err := CopyText(text); err != nil {
		return err
	}

	time.Sleep(clipboardSettleDelay)

	output, err := exec.Command("osascript", "-e", cmdVKeystrokeScript).CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if detail != "" {
			detail = ": " + detail
		}
		return fmt.Errorf("paste keystroke failed (text is on the clipboard, press Cmd+V): %v%s", err, detail)
	}

	return nil
}
