package audio

import (
	"os/exec"

	"github.com/gen2brain/beeep"
)

// PlayBeep plays system beep sound for audio feedback
func PlayBeep(beepType string) {
	// Play system beep sound for audio feedback
	switch beepType {
	case "start":
		// System beep or notification sound
		err := beeep.Beep(beeep.DefaultFreq, beeep.DefaultDuration/2)
		if err != nil {
			// Fallback to system beep command
			exec.Command("osascript", "-e", "beep 1").Run()
		}
	case "stop":
		// Different tone for stop
		err := beeep.Beep(beeep.DefaultFreq*2, beeep.DefaultDuration/3)
		if err != nil {
			// Fallback to system beep command
			exec.Command("osascript", "-e", "beep 2").Run()
		}
	}
}
