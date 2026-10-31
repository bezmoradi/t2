package clipboard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"
)

// PasteKeys is the keystroke that pastes, as the user is told to press it.
// On X11 t2 sends it too. Terminals paste with Ctrl+Shift+V instead, and t2
// cannot tell which app has focus, so the text stays on the clipboard
// either way. On Wayland t2 types the text, which terminals take as well.
const PasteKeys = "Ctrl+V"

// toolTimeout bounds the clipboard tools and xdotool. wl-copy can hang
// on a compositor without a data-control protocol (GNOME), where it falls
// back to briefly showing a window that has to receive focus.
const toolTimeout = 3 * time.Second

// perCharTimeout is what typing is allowed per character, on top of
// toolTimeout. wtype typed about 4.3 ms a character on a Raspberry Pi 5
// (4,000 characters in 17 s) and 6 ms on a software-rendered Sway. Being
// stopped partway leaves a transcript half typed, so this is far above
// both: only a stuck wtype is stopped.
const perCharTimeout = 50 * time.Millisecond

var errTimeout = errors.New("did not finish in time")

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
	wlCopy = tool{"wl-clipboard", []string{"wl-copy"}}
	xclip  = tool{"xclip", []string{"xclip", "-selection", "clipboard", "-in"}}
	xsel   = tool{"xsel", []string{"xsel", "--clipboard", "--input"}}
	// wtype types the text it reads on stdin, which keeps the transcript out
	// of the process list. It doesn't send Ctrl+V: see PasteTextSafely.
	wtype   = tool{"wtype", []string{"wtype", "-"}}
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
// pasted on this desktop, or "" when nothing is missing. A missing clipboard
// tool and a missing keystroke tool are named together, so one run tells the
// user both.
func Limitation() string {
	tools := copyTools()
	found := false
	for _, t := range tools {
		found = found || t.available()
	}
	k := keyTool()
	// wtype only works on compositors with the virtual-keyboard protocol,
	// not on GNOME or KDE, so it is recommended for those compositors only.
	switch {
	case !found && !k.available() && wayland():
		return fmt.Sprintf("no clipboard tool found: install %s, and wtype too on labwc, Sway, Hyprland and other wlroots compositors, so t2 can type transcripts", tools[0].pkg)
	case !found && !k.available():
		return fmt.Sprintf("no clipboard tool found: install %s, and %s too, so t2 can paste", tools[0].pkg, k.pkg)
	case !found:
		return fmt.Sprintf("no clipboard tool found: install %s", tools[0].pkg)
	case !k.available() && wayland():
		return fmt.Sprintf("wtype not found, so transcripts are copied but not typed: press %s yourself (on labwc, Sway, Hyprland and other wlroots compositors, installing wtype lets t2 type them)", PasteKeys)
	case !k.available():
		return fmt.Sprintf("%s not found, so transcripts are copied but not pasted: install %s, or press %s yourself", k.args[0], k.pkg, PasteKeys)
	}
	return ""
}

// run starts a tool with text on its input and waits for it, within
// timeout. env is added to t2's environment for the tool.
//
// Its output is deliberately not captured. The clipboard tools fork a child
// that keeps serving the text until something else is copied, and that child
// inherits any pipe given for output, so collecting stdout or stderr would
// wait for the next copy, not for this one.
func run(t tool, input string, timeout time.Duration, env ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, t.args[0], t.args[1:]...)
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	err := cmd.Run()
	if ctx.Err() != nil {
		return fmt.Errorf("%s %w (%v)", t.args[0], errTimeout, timeout)
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
		err := run(t, text, toolTimeout)
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

// PasteTextSafely copies text to the clipboard and delivers it to the
// focused app: on X11 by sending Ctrl+V, on Wayland by typing it.
//
// Wayland types because GTK3 apps ignore every shortcut wtype sends. wtype
// presses keys from a keymap it generates, which GTK3's shortcut matching
// misses, while the characters it types arrive intact. On labwc and Sway,
// Ctrl+V from wtype pasted nothing into GTK3 apps such as Mousepad; on
// labwc, GTK4 and XWayland apps took it. Typed text reached all of them,
// and terminals too.
//
// Either way the text is delivered once and never retried, as on macOS: a
// keystroke tool can fail after the keys went out, and a second delivery
// corrupts what is being typed.
//
// When the desktop takes no synthetic keystrokes - GNOME and KDE on Wayland
// support neither wtype's protocol nor any other an unprivileged program can
// use - the result is ErrCopiedOnly: the text is on the clipboard and the
// user pastes it.
func PasteTextSafely(text string) error {
	if wayland() {
		return typeText(text)
	}

	if err := CopyText(text); err != nil {
		return err
	}
	if !xdotool.available() {
		return fmt.Errorf("%w (xdotool is not installed)", ErrCopiedOnly)
	}

	// Small delay to ensure clipboard is set
	time.Sleep(100 * time.Millisecond)

	if err := run(xdotool, "", toolTimeout); err != nil {
		return fmt.Errorf("paste keystroke failed (text is on the clipboard, press %s): %v", PasteKeys, err)
	}
	return nil
}

// typeText copies text to the clipboard, as the fallback, and types it with
// wtype. A failed copy doesn't stop the typing.
func typeText(text string) error {
	copyErr := CopyText(text)
	if !wtype.available() {
		if copyErr != nil {
			return copyErr
		}
		return fmt.Errorf("%w (wtype is not installed)", ErrCopiedOnly)
	}

	// wtype decodes its input by the locale. In one that isn't UTF-8, as
	// when LANG is unset, it stops at the first non-ASCII character and
	// still exits 0, so the rest of the text is lost without a word.
	timeout := toolTimeout + time.Duration(utf8.RuneCountInString(text))*perCharTimeout
	err := run(wtype, text, timeout, "LC_ALL=C.UTF-8")
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errTimeout):
		// Stopped partway, so some of the text may already be typed.
		if copyErr != nil {
			return fmt.Errorf("typing was stopped after %v, so the text may be incomplete; it could not be copied either: %v", timeout.Round(time.Second), copyErr)
		}
		return fmt.Errorf("typing was stopped after %v, so the text may be incomplete (the full text is on the clipboard, press %s)", timeout.Round(time.Second), PasteKeys)
	case copyErr != nil:
		return fmt.Errorf("could not type the text (%v), and %v", err, copyErr)
	default:
		// wtype exits at once on a compositor without the
		// virtual-keyboard protocol, before typing anything.
		return fmt.Errorf("%w (%v)", ErrCopiedOnly, err)
	}
}
