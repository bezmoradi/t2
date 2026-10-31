//go:build !linux

package audio

// Limitation reports why recording will not work, for the startup banner.
// Only the Linux backend checks anything before the first hold.
func Limitation() string { return "" }
