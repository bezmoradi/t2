package audio

import (
	"errors"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeStream stands in for the microphone. It can hang in Available or Close
// until released, which is how the containment logic is exercised without
// waiting for the real CoreAudio hang.
type fakeStream struct {
	mu        sync.Mutex
	chunk     []int16
	avail     int
	refill    bool          // Available always reports a full chunk
	availHang chan struct{} // Available blocks until this is closed
	closeHang chan struct{} // Close blocks until this is closed
	closed    int

	entered     chan struct{} // closed on the first Available call
	enteredOnce sync.Once
}

func (f *fakeStream) Available() (int, error) {
	if f.entered != nil {
		f.enteredOnce.Do(func() { close(f.entered) })
	}
	if f.availHang != nil {
		<-f.availHang
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.avail, nil
}

func (f *fakeStream) Read(into []int16) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	copy(into, f.chunk)
	if !f.refill {
		f.avail -= len(into)
	}
	return nil
}

func (f *fakeStream) Overflows() int { return 0 }

func (f *fakeStream) Close() error {
	if f.closeHang != nil {
		<-f.closeHang
	}
	f.mu.Lock()
	f.closed++
	f.mu.Unlock()
	return nil
}

func (f *fakeStream) closes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func useFake(t *testing.T, f *fakeStream) {
	t.Helper()
	prev := OpenStream
	OpenStream = func(int, int) (Stream, error) { return f, nil }
	t.Cleanup(func() { OpenStream = prev })
}

func shorten(t *testing.T, d *time.Duration, to time.Duration) {
	t.Helper()
	prev := *d
	*d = to
	t.Cleanup(func() { *d = prev })
}

// tone is a full chunk whose RMS is exactly amplitude.
func tone(amplitude int16) []int16 {
	c := make([]int16, Frames)
	for i := range c {
		if i%2 == 0 {
			c[i] = amplitude
		} else {
			c[i] = -amplitude
		}
	}
	return c
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatal("condition not met in time")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestCaptureReportsChunksLevelsAndBytes(t *testing.T) {
	f := &fakeStream{chunk: tone(1000), avail: Frames, refill: true}
	useFake(t, f)

	var sent atomic.Int64
	r := NewRecorder(func(b []byte) error { sent.Add(int64(len(b))); return nil })
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	if !r.IsRecording() {
		t.Fatal("not recording after Start")
	}
	time.Sleep(50 * time.Millisecond)

	out := r.Stop()
	if out.Err != nil || out.Wedged {
		t.Fatalf("outcome %+v", out)
	}
	if out.Chunks <= ignoreLeadingChunks {
		t.Fatalf("chunks = %d", out.Chunks)
	}
	if math.Abs(out.MaxRMS-1000) > 1 {
		t.Errorf("MaxRMS = %.1f, want 1000", out.MaxRMS)
	}
	if got, want := sent.Load(), int64(out.Chunks*Frames*2); got != want {
		t.Errorf("sent %d bytes, want %d", got, want)
	}
	if f.closes() != 1 {
		t.Errorf("stream closed %d times, want 1", f.closes())
	}
	if r.IsRecording() || Wedged() {
		t.Error("recorder not idle after a clean stop")
	}
}

// A Close that hangs inside the OS must not hang Stop: it reports Wedged, the
// audio is intact, further Starts are refused, and if the hang ever clears
// the recorder is usable again.
func TestStopReportsWedgedWhenCloseHangs(t *testing.T) {
	shorten(t, &shutdownTimeout, 150*time.Millisecond)
	f := &fakeStream{chunk: tone(10), avail: Frames, refill: true, closeHang: make(chan struct{})}
	useFake(t, f)

	r := NewRecorder(nil)
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)

	started := time.Now()
	out := r.Stop()
	if !out.Wedged {
		t.Fatal("Stop did not report Wedged with a hanging Close")
	}
	if d := time.Since(started); d > time.Second {
		t.Errorf("Stop took %v, want about the %v shutdown timeout", d, shutdownTimeout)
	}
	if out.Err != nil {
		t.Errorf("a wedge is not a capture error, got %v", out.Err)
	}
	if out.Chunks == 0 {
		t.Error("the audio captured before the hang was lost")
	}
	if !Wedged() {
		t.Error("Wedged() = false after a wedged Stop")
	}
	if err := r.Start(); !errors.Is(err, ErrWedged) {
		t.Errorf("Start while wedged = %v, want ErrWedged", err)
	}

	close(f.closeHang)
	waitFor(t, func() bool { return !Wedged() })
	if err := r.Start(); err != nil {
		t.Fatalf("Start after the hang cleared: %v", err)
	}
	r.Stop()
}

func TestStopReportsWedgedWhenTheLoopHangs(t *testing.T) {
	shorten(t, &stopTimeout, 150*time.Millisecond)
	f := &fakeStream{availHang: make(chan struct{}), entered: make(chan struct{})}
	useFake(t, f)

	r := NewRecorder(nil)
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	<-f.entered // the loop is now stuck inside the audio layer

	started := time.Now()
	out := r.Stop()
	if !out.Wedged {
		t.Fatal("Stop did not report Wedged with a hanging read loop")
	}
	if d := time.Since(started); d > time.Second {
		t.Errorf("Stop took %v", d)
	}

	close(f.availHang)
	waitFor(t, func() bool { return !Wedged() })
}

func TestStarvationIsArmedByTheFirstChunk(t *testing.T) {
	shorten(t, &starvationTimeout, 50*time.Millisecond)

	// No audio at all, however long the hold: not an error, just no chunks.
	useFake(t, &fakeStream{})
	r := NewRecorder(nil)
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if out := r.Stop(); out.Err != nil || out.Chunks != 0 {
		t.Fatalf("with no audio ever: %+v, want no error and no chunks", out)
	}

	// One chunk and then nothing: the device died mid-recording.
	useFake(t, &fakeStream{chunk: tone(10), avail: Frames})
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	out := r.Stop()
	if out.Err == nil || !strings.Contains(out.Err.Error(), "stopped delivering") {
		t.Fatalf("after audio stopped: err = %v, want a starvation error", out.Err)
	}
	if out.Chunks != 1 {
		t.Errorf("chunks = %d, want 1", out.Chunks)
	}
}

func TestSendErrorEndsTheCapture(t *testing.T) {
	f := &fakeStream{chunk: tone(10), avail: Frames, refill: true}
	useFake(t, f)

	boom := errors.New("socket closed")
	r := NewRecorder(func([]byte) error { return boom })
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)

	if out := r.Stop(); !errors.Is(out.Err, boom) {
		t.Fatalf("err = %v, want the send error", out.Err)
	}
	if f.closes() != 1 {
		t.Errorf("stream closed %d times, want 1", f.closes())
	}
}
