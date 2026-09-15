package app

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/bezmoradi/t2/internal/audio"
	"github.com/bezmoradi/t2/internal/hotkeys"
	"github.com/bezmoradi/t2/internal/transcription"
)

// hangingStream never delivers audio and hangs in Close until released: the
// shape of the CoreAudio hang, without the hardware.
type hangingStream struct{ release chan struct{} }

func (h *hangingStream) Available() (int, error) { return 0, nil }
func (h *hangingStream) Read([]int16) error      { return nil }
func (h *hangingStream) Overflows() int          { return 0 }
func (h *hangingStream) Close() error            { <-h.release; return nil }

type restartSpy struct {
	mu    sync.Mutex
	times []time.Time
}

func (s *restartSpy) hit() {
	s.mu.Lock()
	s.times = append(s.times, time.Now())
	s.mu.Unlock()
}

func (s *restartSpy) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.times)
}

// newQuietDaemon builds a daemon with no beeps, no network, no pasting and a
// restart spy, on the given stream.
func newQuietDaemon(t *testing.T, stream audio.Stream) (*Daemon, *restartSpy) {
	t.Helper()
	prev := audio.OpenStream
	audio.OpenStream = func(int, int) (audio.Stream, error) { return stream, nil }
	t.Cleanup(func() { audio.OpenStream = prev })

	spy := &restartSpy{}
	d := NewDaemon(false)
	d.recorder = audio.NewRecorder(d.sink.write)
	d.beep = func(string) {}
	d.dial = func(string, int) (*transcription.Session, error) { return nil, errors.New("no network in tests") }
	d.paste = func(string) error { return nil }
	d.copy = func(string) error { return nil }
	d.restart = spy.hit
	return d, spy
}

func setGrace(t *testing.T, d time.Duration) {
	t.Helper()
	prev := restartGrace
	restartGrace = d
	t.Cleanup(func() { restartGrace = prev })
}

func waitForCount(t *testing.T, spy *restartSpy, want int, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for spy.count() < want {
		select {
		case <-deadline:
			t.Fatalf("restarts = %d, want %d within %v", spy.count(), want, within)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// A stop that hangs must not restart during the grace period, must restart
// at once if the user presses again meanwhile, and must restart on its own
// once the grace period is over.
func TestWedgeRestartsAfterGraceAndAtOnceOnAPress(t *testing.T) {
	setGrace(t, 600*time.Millisecond)
	stream := &hangingStream{release: make(chan struct{})}
	d, spy := newQuietDaemon(t, stream)

	d.OnPress()
	time.Sleep(30 * time.Millisecond)
	wedgedAt := time.Now()
	d.OnRelease(hotkeys.Release{Held: 30 * time.Millisecond})
	if !audio.Wedged() {
		t.Fatal("the daemon did not detect the hanging stop")
	}
	if d.recorder.IsRecording() {
		t.Fatal("still recording after a wedged release")
	}

	time.Sleep(restartGrace / 2)
	if n := spy.count(); n != 0 {
		t.Fatalf("restarted %d times inside the grace period", n)
	}

	d.OnPress()
	waitForCount(t, spy, 1, 500*time.Millisecond)
	if d.recorder.IsRecording() {
		t.Fatal("a press while wedged started a recording")
	}

	waitForCount(t, spy, 2, restartGrace+time.Second)
	t.Logf("scheduled restart fired %v after the wedge", time.Since(wedgedAt).Round(time.Millisecond))

	close(stream.release)
	deadline := time.Now().Add(3 * time.Second)
	for audio.Wedged() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
}

// A stop that was merely slow, and completes inside the grace period, must
// not cost a restart.
func TestWedgeThatClearsInTimeDoesNotRestart(t *testing.T) {
	setGrace(t, time.Second)
	stream := &hangingStream{release: make(chan struct{})}
	d, spy := newQuietDaemon(t, stream)

	d.OnPress()
	time.Sleep(30 * time.Millisecond)
	d.OnRelease(hotkeys.Release{Held: 30 * time.Millisecond})
	if !audio.Wedged() {
		t.Fatal("the daemon did not detect the hanging stop")
	}

	close(stream.release)
	time.Sleep(restartGrace + 500*time.Millisecond)

	if n := spy.count(); n != 0 {
		t.Fatalf("restarted %d times although the stop completed within the grace period", n)
	}
	if audio.Wedged() {
		t.Fatal("still wedged after the stream closed")
	}
}
