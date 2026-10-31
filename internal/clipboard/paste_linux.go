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

const PasteKeys = "Ctrl+V"

const (
	toolTimeout          = 3 * time.Second
	typingTimeoutPerChar = 50 * time.Millisecond
	clipboardSettleDelay = 100 * time.Millisecond
	forceUTF8Locale      = "LC_ALL=C.UTF-8"
)

var errToolTimedOut = errors.New("did not finish in time")

func waylandSession() bool { return os.Getenv("WAYLAND_DISPLAY") != "" }

type tool struct {
	packageName string
	args        []string
}

func (t tool) available() bool {
	_, err := exec.LookPath(t.args[0])
	return err == nil
}

var (
	wlCopy             = tool{"wl-clipboard", []string{"wl-copy"}}
	xclip              = tool{"xclip", []string{"xclip", "-selection", "clipboard", "-in"}}
	xsel               = tool{"xsel", []string{"xsel", "--clipboard", "--input"}}
	wtypeTextFromStdin = tool{"wtype", []string{"wtype", "-"}}
	xdotoolCtrlV       = tool{"xdotool", []string{"xdotool", "key", "--clearmodifiers", "ctrl+v"}}
)

func copyTools() []tool {
	if waylandSession() {
		return []tool{wlCopy}
	}
	return []tool{xclip, xsel}
}

func keystrokeTool() tool {
	if waylandSession() {
		return wtypeTextFromStdin
	}
	return xdotoolCtrlV
}

func Limitation() string {
	tools := copyTools()
	canCopy := false
	for _, t := range tools {
		canCopy = canCopy || t.available()
	}
	k := keystrokeTool()
	switch {
	case !canCopy && !k.available() && waylandSession():
		return fmt.Sprintf("no clipboard tool found: install %s, and wtype too on labwc, Sway, Hyprland and other wlroots compositors, so t2 can type transcripts", tools[0].packageName)
	case !canCopy && !k.available():
		return fmt.Sprintf("no clipboard tool found: install %s, and %s too, so t2 can paste", tools[0].packageName, k.packageName)
	case !canCopy:
		return fmt.Sprintf("no clipboard tool found: install %s", tools[0].packageName)
	case !k.available() && waylandSession():
		return fmt.Sprintf("wtype not found, so transcripts are copied but not typed: press %s yourself (on labwc, Sway, Hyprland and other wlroots compositors, installing wtype lets t2 type them)", PasteKeys)
	case !k.available():
		return fmt.Sprintf("%s not found, so transcripts are copied but not pasted: install %s, or press %s yourself", k.args[0], k.packageName, PasteKeys)
	}
	return ""
}

func runWithoutCapturingOutput(t tool, input string, timeout time.Duration, extraEnv ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, t.args[0], t.args[1:]...)
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	err := cmd.Run()
	if ctx.Err() != nil {
		return fmt.Errorf("%s %w (%v)", t.args[0], errToolTimedOut, timeout)
	}
	if err != nil {
		return fmt.Errorf("%s: %v", t.args[0], err)
	}
	return nil
}

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
		err := runWithoutCapturingOutput(t, text, toolTimeout)
		if err == nil {
			return nil
		}
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		return fmt.Errorf("failed to copy to clipboard: no clipboard tool found, install %s", tools[0].packageName)
	}
	return fmt.Errorf("failed to copy to clipboard: %v", errors.Join(errs...))
}

func PasteOnce(text string) error {
	if waylandSession() {
		return copyThenType(text)
	}

	if err := CopyText(text); err != nil {
		return err
	}
	if !xdotoolCtrlV.available() {
		return fmt.Errorf("%w (xdotool is not installed)", ErrCopiedOnly)
	}

	time.Sleep(clipboardSettleDelay)

	if err := runWithoutCapturingOutput(xdotoolCtrlV, "", toolTimeout); err != nil {
		return fmt.Errorf("paste keystroke failed (text is on the clipboard, press %s): %v", PasteKeys, err)
	}
	return nil
}

func copyThenType(text string) error {
	copyErr := CopyText(text)
	if !wtypeTextFromStdin.available() {
		if copyErr != nil {
			return copyErr
		}
		return fmt.Errorf("%w (wtype is not installed)", ErrCopiedOnly)
	}

	typingTimeout := toolTimeout + time.Duration(utf8.RuneCountInString(text))*typingTimeoutPerChar
	err := runWithoutCapturingOutput(wtypeTextFromStdin, text, typingTimeout, forceUTF8Locale)
	stoppedPartway := errors.Is(err, errToolTimedOut)
	switch {
	case err == nil:
		return nil
	case stoppedPartway && copyErr != nil:
		return fmt.Errorf("typing was stopped after %v, so the text may be incomplete; it could not be copied either: %v", typingTimeout.Round(time.Second), copyErr)
	case stoppedPartway:
		return fmt.Errorf("typing was stopped after %v, so the text may be incomplete (the full text is on the clipboard, press %s)", typingTimeout.Round(time.Second), PasteKeys)
	case copyErr != nil:
		return fmt.Errorf("could not type the text (%v), and %v", err, copyErr)
	default:
		return fmt.Errorf("%w (%v)", ErrCopiedOnly, err)
	}
}
