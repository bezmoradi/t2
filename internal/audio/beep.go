package audio

import "os/exec"

// PlayBeep plays a short feedback sound. It does not block.
//
// This used to call beeep.Beep, which on macOS ignores its frequency and
// duration arguments entirely and shells out to `osascript -e beep`
// synchronously: ~100ms of dead time on the press and release paths, twice per
// recording, for two identical sounds where the code claimed to play different
// tones for start and stop.
func PlayBeep(beepType string) {
	sound := "/System/Library/Sounds/Tink.aiff"
	if beepType == "stop" {
		sound = "/System/Library/Sounds/Pop.aiff"
	}

	cmd := exec.Command("afplay", sound)
	if err := cmd.Start(); err != nil {
		return
	}

	// Reap the child without waiting on it here.
	go cmd.Wait()
}
