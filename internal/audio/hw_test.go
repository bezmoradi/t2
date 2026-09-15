package audio

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestAudioQueueStressUnderChurn is the test that PortAudio failed: hundreds
// of open/capture/close cycles with 32-frame buffers, which multiplies IO
// cycles and stop handshakes, while other CoreAudio clients keep starting and
// stopping in the background (a silent file played by afplay in a loop).
// That churn is what delayed AUHAL's stop acknowledgement and deadlocked
// PortAudio's Pa_StopStream within a few dozen cycles.
//
// Needs a microphone:
//
//	T2_HW_TESTS=1 go test ./internal/audio -run TestAudioQueueStressUnderChurn -v -timeout 10m
func TestAudioQueueStressUnderChurn(t *testing.T) {
	if os.Getenv("T2_HW_TESTS") == "" {
		t.Skip("set T2_HW_TESTS=1 to run hardware tests")
	}
	t.Setenv(framesEnv, "32")

	silent := filepath.Join(t.TempDir(), "silent.wav")
	if err := WriteWAV(silent, make([]byte, SampleRate*2/5)); err != nil {
		t.Fatal(err)
	}
	stopChurn := make(chan struct{})
	churnDone := make(chan struct{})
	go func() {
		defer close(churnDone)
		for {
			select {
			case <-stopChurn:
				return
			default:
			}
			exec.Command("afplay", silent).Run()
		}
	}()
	defer func() { close(stopChurn); <-churnDone }()

	const cycles = 300
	var stopMax, stopSum time.Duration
	chunks := 0
	r := NewRecorder(nil)
	start := time.Now()
	for i := 1; i <= cycles; i++ {
		if err := r.Start(); err != nil {
			t.Fatalf("cycle %d start: %v", i, err)
		}
		time.Sleep(150 * time.Millisecond)

		st := time.Now()
		out := r.Stop()
		d := time.Since(st)
		stopMax = max(stopMax, d)
		stopSum += d
		chunks += out.Chunks

		if out.Wedged {
			t.Fatalf("cycle %d: the AudioQueue backend hung on close (Stop gave up after %v)", i, d)
		}
		if out.Err != nil {
			t.Fatalf("cycle %d: %v", i, out.Err)
		}
		if out.Chunks == 0 {
			t.Fatalf("cycle %d: no audio delivered", i)
		}
	}
	t.Logf("%d cycles with 32-frame buffers under client churn in %v: no hang, %d chunks, Stop avg %v max %v",
		cycles, time.Since(start).Round(time.Second), chunks, (stopSum / cycles).Round(time.Millisecond), stopMax.Round(time.Millisecond))
}
