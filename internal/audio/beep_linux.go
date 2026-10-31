package audio

import (
	"bytes"
	"math"
	"os/exec"
	"sync"
	"syscall"
)

// PlayBeep plays a short feedback tone. It does not block.
//
// Linux has no sound file every desktop ships, so the tones are generated
// and played through pacat, which comes in the same package as parecord:
// a higher one for start, a lower one for stop.
func PlayBeep(beepType string) {
	pcm := startTone()
	if beepType == "stop" {
		pcm = stopTone()
	}

	cmd := exec.Command("pacat", "--playback", "--raw", "--format=s16le",
		"--rate=16000", "--channels=1", "--client-name=t2", "--stream-name=Beep")
	cmd.Stdin = bytes.NewReader(pcm)
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		return
	}

	// Reap the child without waiting on it here.
	go cmd.Wait()
}

var (
	startTone = sync.OnceValue(func() []byte { return beepTone(880, 0.08) })
	stopTone  = sync.OnceValue(func() []byte { return beepTone(587, 0.08) })
)

// beepTone is a sine at freq Hz lasting secs, as 16 kHz mono s16le, faded in and
// out over 5ms so it does not click.
func beepTone(freq, secs float64) []byte {
	const rate = 16000
	n := int(secs * rate)
	fade := rate * 5 / 1000
	out := make([]byte, 2*n)
	for i := range n {
		gain := 0.25
		if i < fade {
			gain *= float64(i) / float64(fade)
		} else if n-i < fade {
			gain *= float64(n-i) / float64(fade)
		}
		v := int16(gain * 32767 * math.Sin(2*math.Pi*freq*float64(i)/rate))
		out[2*i] = byte(v)
		out[2*i+1] = byte(v >> 8)
	}
	return out
}
