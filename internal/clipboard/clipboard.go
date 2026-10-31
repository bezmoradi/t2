package clipboard

import "errors"

var ErrCopiedOnly = errors.New("copied to the clipboard; this desktop accepts no synthetic keystrokes")
