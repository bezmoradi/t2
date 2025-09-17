package terminal

import (
	"fmt"
	"os"
	"runtime"
)

// Control provides terminal control functionality
type Control struct {
	isWindows bool
}

// NewControl creates a new terminal control instance
func NewControl() *Control {
	return &Control{
		isWindows: runtime.GOOS == "windows",
	}
}

// MoveCursorUp moves the cursor up by the specified number of lines
func (c *Control) MoveCursorUp(lines int) {
	if lines <= 0 {
		return
	}

	if c.isWindows {
		// Windows ANSI escape sequence
		fmt.Printf("\033[%dA", lines)
	} else {
		// Unix/Linux/macOS ANSI escape sequence
		fmt.Printf("\033[%dA", lines)
	}
}

// ClearLine clears the current line
func (c *Control) ClearLine() {
	if c.isWindows {
		fmt.Print("\033[2K\r")
	} else {
		fmt.Print("\033[2K\r")
	}
}

// ClearFromCursor clears from cursor to end of line
func (c *Control) ClearFromCursor() {
	fmt.Print("\033[K")
}

// ClearLines clears the specified number of lines starting from current position
func (c *Control) ClearLines(count int) {
	for i := 0; i < count; i++ {
		c.ClearLine()
		if i < count-1 {
			c.MoveCursorUp(1)
		}
	}
}

// MoveCursorToColumn moves cursor to the specified column (1-based)
func (c *Control) MoveCursorToColumn(col int) {
	fmt.Printf("\033[%dG", col)
}

// SaveCursor saves the current cursor position
func (c *Control) SaveCursor() {
	fmt.Print("\033[s")
}

// RestoreCursor restores the saved cursor position
func (c *Control) RestoreCursor() {
	fmt.Print("\033[u")
}

// IsTerminal checks if output is going to a terminal
func (c *Control) IsTerminal() bool {
	// Check if stdout is a terminal
	fileInfo, err := os.Stdout.Stat()
	if err != nil {
		return false
	}

	// On Unix-like systems, check if it's a character device
	return (fileInfo.Mode() & os.ModeCharDevice) != 0
}

// UpdateInPlace updates multiple lines in place
// This is the main function for dynamically updating session output
func (c *Control) UpdateInPlace(lines []string, isFirstUpdate bool) {
	if !c.IsTerminal() {
		// If not in a terminal (e.g., piped output), just print normally
		for _, line := range lines {
			fmt.Println(line)
		}
		return
	}

	if !isFirstUpdate {
		// Move cursor up to overwrite previous output
		c.MoveCursorUp(len(lines))
	}

	// Print each line, clearing it first if not the first update
	for i, line := range lines {
		if !isFirstUpdate {
			c.ClearLine()
		}
		fmt.Print(line)

		// Add newline except for the last line
		if i < len(lines)-1 {
			fmt.Println()
		}
	}

	// Ensure we end with a newline for proper positioning
	if isFirstUpdate {
		fmt.Println()
	}
}

// HideCursor hides the terminal cursor
func (c *Control) HideCursor() {
	fmt.Print("\033[?25l")
}

// ShowCursor shows the terminal cursor
func (c *Control) ShowCursor() {
	fmt.Print("\033[?25h")
}
