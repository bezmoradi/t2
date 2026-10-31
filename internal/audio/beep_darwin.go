package audio

import "os/exec"

const (
	startSound = "/System/Library/Sounds/Tink.aiff"
	stopSound  = "/System/Library/Sounds/Pop.aiff"
)

func PlayBeep(beepType string) {
	sound := startSound
	if beepType == "stop" {
		sound = stopSound
	}

	cmd := exec.Command("afplay", sound)
	if err := cmd.Start(); err != nil {
		return
	}
	go cmd.Wait()
}
