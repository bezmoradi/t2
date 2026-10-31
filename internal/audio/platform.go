package audio

import "runtime"

// Backend names what capture runs through, for messages about it.
func Backend() string {
	if runtime.GOOS == "linux" {
		return "parecord"
	}
	return "CoreAudio"
}

// SoundSettings is where the user chooses the microphone.
func SoundSettings() string {
	if runtime.GOOS == "linux" {
		return "your desktop's sound settings (or pavucontrol)"
	}
	return "System Settings > Sound"
}
