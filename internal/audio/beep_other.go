//go:build !darwin && !linux

package audio

// PlayBeep does nothing where there is no sound tool to call.
func PlayBeep(beepType string) {}
