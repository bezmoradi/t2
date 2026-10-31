//go:build !unix

package app

import "os"

// LockInstance is a no-op where flock is unavailable; t2 does not run there.
func LockInstance() (*os.File, error) { return nil, nil }
