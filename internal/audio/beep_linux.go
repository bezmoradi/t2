package audio

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os/exec"
	"sync"
	"time"
)

const (
	toneSampleRate = 16000
	startToneHz    = 880
	stopToneHz     = 587
	toneLength     = 80 * time.Millisecond
	toneFade       = 5 * time.Millisecond
	toneVolume     = 0.25
)

func PlayBeep(beepType string) {
	pcm := startTone()
	if beepType == "stop" {
		pcm = stopTone()
	}

	cmd := exec.Command("pacat", "--playback", "--raw", "--format=s16le",
		fmt.Sprintf("--rate=%d", toneSampleRate), "--channels=1", "--client-name=t2", "--stream-name=Beep")
	cmd.Stdin = bytes.NewReader(pcm)
	cmd.SysProcAttr = killedWithT2()
	if err := cmd.Start(); err != nil {
		return
	}
	go cmd.Wait()
}

var (
	startTone = sync.OnceValue(func() []byte { return fadedSineS16LE(startToneHz, toneLength) })
	stopTone  = sync.OnceValue(func() []byte { return fadedSineS16LE(stopToneHz, toneLength) })
)

func fadedSineS16LE(hz float64, length time.Duration) []byte {
	n := int(length.Seconds() * toneSampleRate)
	fadeSamples := int(toneFade.Seconds() * toneSampleRate)
	out := make([]byte, BytesPerSample*n)
	for i := range n {
		gain := toneVolume
		if i < fadeSamples {
			gain *= float64(i) / float64(fadeSamples)
		} else if n-i < fadeSamples {
			gain *= float64(n-i) / float64(fadeSamples)
		}
		v := int16(gain * math.MaxInt16 * math.Sin(2*math.Pi*hz*float64(i)/toneSampleRate))
		binary.LittleEndian.PutUint16(out[BytesPerSample*i:], uint16(v))
	}
	return out
}
