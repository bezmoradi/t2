package clipboard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// PasteKeys is the keystroke that pastes, as the user is told to press it.
// Terminals paste with Ctrl+Shift+V instead; t2 cannot tell which app has
// focus, so it sends Ctrl+V and the text stays on the clipboard either way.
const PasteKeys = "Ctrl+V"

// toolTimeout bounds every clipboard and keystroke tool. wl-copy can hang
// on a compositor without a data-control protocol (GNOME), where it falls
// back to briefly showing a window that has to receive focus.
const toolTimeout = 3 * time.Second

// wayland reports whether this is a Wayland session. X11 tools still run
// there, through XWayland, but only reach X11 windows.
func wayland() bool { return os.Getenv("WAYLAND_DISPLAY") != "" }

// tool is a command line with the package that provides it.
type tool struct {
	pkg  string
	args []string
}

func (t tool) available() bool {
	_, err := exec.LookPath(t.args[0])
	return err == nil
}

var (
	wlCopy  = tool{"wl-clipboard", []string{"wl-copy"}}
	xclip   = tool{"xclip", []string{"xclip", "-selection", "clipboard", "-in"}}
	xsel    = tool{"xsel", []string{"xsel", "--clipboard", "--input"}}
	wtype   = tool{"wtype", []string{"wtype", "-M", "ctrl", "-k", "v", "-m", "ctrl"}}
	xdotool = tool{"xdotool", []string{"xdotool", "key", "--clearmodifiers", "ctrl+v"}}
)

func copyTools() []tool {
	if wayland() {
		return []tool{wlCopy}
	}
	return []tool{xclip, xsel}
}

func keyTool() tool {
	if wayland() {
		return wtype
	}
	return xdotool
}

// Limitation reports, for the startup banner, why transcripts will not be
// pasted on this desktop, or "" when nothing is missing.
func Limitation() string {
	tools := copyTools()
	found := false
	for _, t := range tools {
		found = found || t.available()
	}
	if !found {
		return fmt.Sprintf("no clipboard tool found: install %s", tools[0].pkg)
	}
	if k := keyTool(); !k.available() {
		if wayland() {
			// wtype only works on compositors with the virtual-keyboard
			// protocol, so it is not worth recommending on GNOME or KDE.
			return fmt.Sprintf("wtype not found, so transcripts are copied but not pasted: press %s yourself (on Sway, Hyprland and other wlroots compositors, installing wtype lets t2 paste)", PasteKeys)
		}
		return fmt.Sprintf("%s not found, so transcripts are copied but not pasted: install %s, or press %s yourself", k.args[0], k.pkg, PasteKeys)
	}
	return ""
}

// run starts a tool with text on its input and waits for it, within
// toolTimeout.
//
// Its output is deliberately not captured. The clipboard tools fork a child
// that keeps serving the text until something else is copied, and that child
// inherits any pipe given for output, so collecting stdout or stderr would
// wait for the next copy, not for this one.
func run(t tool, input string) error {
	ctx, cancel := context.WithTimeout(context.Background(), toolTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, t.args[0], t.args[1:]...)
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}
	err := cmd.Run()
	if ctx.Err() != nil {
		return fmt.Errorf("%s did not finish within %v", t.args[0], toolTimeout)
	}
	if err != nil {
		return fmt.Errorf("%s: %v", t.args[0], err)
	}
	return nil
}

// CopyText puts text on the clipboard without pasting it.
func CopyText(text string) error {
	if text == "" {
		return fmt.Errorf("empty text")
	}

	tools := copyTools()
	var errs []error
	for _, t := range tools {
		if !t.available() {
			continue
		}
		err := run(t, text)
		if err == nil {
			return nil
		}
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		return fmt.Errorf("failed to copy to clipboard: no clipboard tool found, install %s", tools[0].pkg)
	}
	return fmt.Errorf("failed to copy to clipboard: %v", errors.Join(errs...))
}

// PasteTextSafely copies text to the clipboard and sends Ctrl+V.
//
// When the desktop takes no synthetic keystrokes - GNOME and KDE on Wayland
// support neither wtype's protocol nor any other an unprivileged program can
// use - the result is ErrCopiedOnly: the text is on the clipboard and the
// user pastes it.
func PasteTextSafely(text string) error {
	if err := CopyText(text); err != nil {
		return err
	}

	k := keyTool()
	if !k.available() {
		return fmt.Errorf("%w (%s is not installed)", ErrCopiedOnly, k.args[0])
	}

	// Small delay to ensure clipboard is set
	time.Sleep(100 * time.Millisecond)

	// Sent exactly once, as on macOS: a keystroke tool can fail after the
	// keystroke went out, and a second paste corrupts what is being typed.
	if err := run(k, ""); err != nil {
		if wayland() {
			// wtype exits at once on a compositor without the
			// virtual-keyboard protocol, before sending anything.
			return fmt.Errorf("%w (%v)", ErrCopiedOnly, err)
		}
		return fmt.Errorf("paste keystroke failed (text is on the clipboard, press %s): %v", PasteKeys, err)
	}
	return nil
}
