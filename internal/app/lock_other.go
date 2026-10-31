//go:build !unix

package app

import "os"

func LockInstance() (heldUntilClosed *os.File, err error) { return nil, nil }
