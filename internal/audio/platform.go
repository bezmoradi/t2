package audio

import "runtime"

func Backend() string {
	if runtime.GOOS == "linux" {
		return "parecord"
	}
	return "CoreAudio"
}

func SoundSettings() string {
	if runtime.GOOS == "linux" {
		return "your desktop's sound settings (or pavucontrol)"
	}
	return "System Settings > Sound"
}
