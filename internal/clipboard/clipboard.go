// Package clipboard delivers a transcript to the focused application.
package clipboard

import "errors"

// ErrCopiedOnly reports that the text is on the clipboard but could not be
// pasted, because this desktop gives t2 no way to send the keystroke. The
// user pastes it with PasteKeys.
var ErrCopiedOnly = errors.New("copied to the clipboard; this desktop accepts no synthetic keystrokes")
