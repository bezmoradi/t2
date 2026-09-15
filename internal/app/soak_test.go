//go:build darwin

package app

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bezmoradi/t2/internal/audio"
	"github.com/bezmoradi/t2/internal/hotkeys"
)

// TestSoak runs the real press/release pipeline - microphone, AssemblyAI
// session, transcript - hundreds of times in a row and reports process memory,
// goroutines, threads, stray child processes and per-release latency at
// checkpoints, so that anything growing with use shows up as a trend. Speech
// comes from macOS text-to-speech through the speakers, which the microphone
// hears, so it is hands-off; the paste step is stubbed so nothing is typed
// into whatever window is focused.
//
// It is skipped unless asked for, because it needs a microphone, an API key,
// and bills session time at about $0.15 per hour of it:
//
//	T2_SOAK_CYCLES=200 T2_SOAK_HOLD=10s go test ./internal/app -run TestSoak -v -timeout 2h
func TestSoak(t *testing.T) {
	cycles, _ := strconv.Atoi(os.Getenv("T2_SOAK_CYCLES"))
	if cycles == 0 {
		t.Skip("set T2_SOAK_CYCLES to run the soak test")
	}

	hold := 10 * time.Second
	if v := os.Getenv("T2_SOAK_HOLD"); v != "" {
		var err error
		if hold, err = time.ParseDuration(v); err != nil {
			t.Fatalf("T2_SOAK_HOLD: %v", err)
		}
	}

	every := 20
	if v, _ := strconv.Atoi(os.Getenv("T2_SOAK_EVERY")); v > 0 {
		every = v
	}

	d := NewDaemon(true)
	if err := d.Initialize(); err != nil {
		t.Fatal(err)
	}
	d.paste = func(string) error { return nil }
	d.copy = func(string) error { return nil }
	d.restart = func() { t.Log("daemon asked for a restart (suppressed in the soak test)") }

	const text = "The quick brown fox jumps over the lazy dog while the committee reviews " +
		"the quarterly budget and schedules a follow up meeting for next Tuesday afternoon. " +
		"Please remember to bring the updated figures and the revised timeline."

	var releaseTotal time.Duration
	report := func(label string, n int) {
		runtime.GC()
		debug.FreeOSMemory()
		var avg time.Duration
		if n > 0 {
			avg = releaseTotal / time.Duration(n)
		}
		t.Logf("%-9s rss=%dKB goroutines=%d threads=%d afplay=%d sinkCap=%dKB avgRelease=%v",
			label, rssKB(t), runtime.NumGoroutine(), threads(t), afplays(),
			sinkCapacity(d.sink)/1024, avg.Round(time.Millisecond))
	}

	report("start", 0)
	for i := 1; i <= cycles; i++ {
		speak := exec.Command("say", "-r", "200", text)
		if err := speak.Start(); err != nil {
			t.Fatalf("say: %v", err)
		}

		d.OnPress()
		time.Sleep(hold)

		started := time.Now()
		d.OnRelease(hotkeys.Release{Held: hold})
		releaseTotal += time.Since(started)

		speak.Process.Kill()
		speak.Wait()

		if audio.Wedged() {
			t.Logf("cycle %d: PortAudio stop deadlocked - this process's audio is wedged", i)
			probeNewStreamAfterWedge(t)
			return
		}

		if i%every == 0 {
			report("after "+strconv.Itoa(i), every)
			releaseTotal = 0
		}
	}
}

func rssKB(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

func threads(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("ps", "-M", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		t.Fatal(err)
	}
	return len(strings.Split(strings.TrimSpace(string(out)), "\n")) - 1
}

// afplays counts beep players still alive; a hung one is a leaked process.
func afplays() int {
	out, _ := exec.Command("pgrep", "-x", "afplay").Output()
	return len(strings.Fields(string(out)))
}

// probeNewStreamAfterWedge answers whether a wedged process can still record:
// it opens a fresh stream directly, with a timeout.
func probeNewStreamAfterWedge(t *testing.T) {
	result := make(chan string, 1)
	go func() {
		st := time.Now()
		s, err := audio.OpenStream(audio.SampleRate, audio.Frames)
		if err != nil {
			result <- "open FAILED: " + err.Error()
			return
		}
		in := make([]int16, audio.Frames)
		got := 0
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && got < 5 {
			if n, _ := s.Available(); n >= audio.Frames {
				s.Read(in)
				got++
			} else {
				time.Sleep(5 * time.Millisecond)
			}
		}
		s.Close()
		result <- fmt.Sprintf("open OK in %v, chunks read %d", time.Since(st).Round(time.Millisecond), got)
	}()
	select {
	case r := <-result:
		t.Logf("new stream after wedge: %s", r)
	case <-time.After(8 * time.Second):
		t.Logf("new stream after wedge: HUNG - the process cannot record again until it restarts")
	}
}

// sinkCapacity is the backing capacity of the current recording's buffer.
func sinkCapacity(a *audioSink) int {
	r := a.current()
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return cap(r.all)
}
