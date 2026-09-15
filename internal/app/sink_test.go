package app

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeSender struct {
	mu     sync.Mutex
	frames [][]byte
	block  chan struct{} // when non-nil, Send blocks until it is closed
	fail   error
}

func (f *fakeSender) Send(pcm []byte) error {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.frames = append(f.frames, append([]byte(nil), pcm...))
	return nil
}

func (f *fakeSender) received() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []byte
	for _, fr := range f.frames {
		out = append(out, fr...)
	}
	return out
}

func chunkOf(i int) []byte {
	c := make([]byte, 2048)
	for j := range c {
		c[j] = byte(i)
	}
	return c
}

// Audio captured before the session exists, and audio captured after, must
// all reach the session exactly once, in order, in frames the API accepts.
func TestSinkDeliversEverythingInOrderInBoundedFrames(t *testing.T) {
	a := newAudioSink()
	a.start()
	defer a.stop()

	var want []byte
	for i := 0; i < 50; i++ { // ~3s buffered while "dialling"
		a.write(chunkOf(i))
		want = append(want, chunkOf(i)...)
	}

	f := &fakeSender{}
	a.attach(f)

	for i := 50; i < 80; i++ { // live streaming
		a.write(chunkOf(i))
		want = append(want, chunkOf(i)...)
	}

	a.flush(2 * time.Second)

	if got := f.received(); !bytes.Equal(got, want) {
		t.Fatalf("delivered %d bytes, want %d, or out of order", len(got), len(want))
	}
	for _, fr := range f.frames {
		if len(fr) == 0 || len(fr) > maxFrameBytes {
			t.Errorf("frame of %d bytes, want 1..%d", len(fr), maxFrameBytes)
		}
	}
	if err := a.err(); err != nil {
		t.Errorf("err() = %v", err)
	}
	if !bytes.Equal(a.audio(), want) {
		t.Error("audio() does not hold the whole recording")
	}
}

// The capture goroutine calls write; it must never wait on the network. This
// is the property that used to be violated: a stalled WebSocket write held the
// capture for its 5s deadline, overflowed PortAudio's ring buffer, and made a
// Stop during the stall look like the CoreAudio deadlock.
func TestSinkWriteNeverBlocksOnAStalledSend(t *testing.T) {
	a := newAudioSink()
	a.start()
	defer a.stop()

	f := &fakeSender{block: make(chan struct{})}
	a.attach(f)
	a.write(chunkOf(0))
	time.Sleep(20 * time.Millisecond) // the sender is now stuck inside Send

	started := time.Now()
	for i := 1; i <= 100; i++ {
		a.write(chunkOf(i))
	}
	if d := time.Since(started); d > 100*time.Millisecond {
		t.Fatalf("100 writes took %v while the send was stalled; write must not block", d)
	}

	started = time.Now()
	a.flush(200 * time.Millisecond)
	if d := time.Since(started); d < 150*time.Millisecond || d > time.Second {
		t.Errorf("flush during a stall returned after %v, want about its 200ms timeout", d)
	}

	close(f.block)
	a.flush(2 * time.Second)
	if got, want := len(f.received()), 101*2048; got != want {
		t.Errorf("delivered %d bytes after the stall cleared, want %d", got, want)
	}
}

func TestSinkStopsSendingAfterAFailure(t *testing.T) {
	a := newAudioSink()
	a.start()
	defer a.stop()

	f := &fakeSender{fail: errors.New("socket closed")}
	a.attach(f)
	a.write(chunkOf(0))
	a.write(chunkOf(1))
	a.flush(time.Second)

	if a.err() == nil {
		t.Fatal("err() = nil after the sender failed")
	}
	if got := len(a.audio()); got != 2*2048 {
		t.Errorf("audio() has %d bytes, want %d: the recording must survive a send failure", got, 2*2048)
	}
}

// A sender still stuck in a slow write when the next recording starts must
// not be able to touch the next recording.
func TestSinkNewRecordingIsIsolatedFromAStalledSender(t *testing.T) {
	a := newAudioSink()
	a.start()

	stalled := &fakeSender{block: make(chan struct{})}
	a.attach(stalled)
	a.write(chunkOf(1))
	time.Sleep(20 * time.Millisecond)

	started := time.Now()
	a.stop() // must return without waiting out the stall
	if d := time.Since(started); d > 1500*time.Millisecond {
		t.Fatalf("stop took %v with a stalled sender", d)
	}

	a.start()
	next := &fakeSender{}
	a.attach(next)
	a.write(chunkOf(2))
	a.flush(time.Second)

	close(stalled.block) // the old sender wakes up and finishes
	time.Sleep(50 * time.Millisecond)

	if !bytes.Equal(a.audio(), chunkOf(2)) {
		t.Error("the new recording's audio was disturbed")
	}
	if !bytes.Equal(next.received(), chunkOf(2)) {
		t.Errorf("the new session received %d bytes, want exactly the new chunk", len(next.received()))
	}
	if !bytes.Equal(stalled.received(), chunkOf(1)) {
		t.Errorf("the old session received %d bytes, want exactly its own chunk", len(stalled.received()))
	}
	a.stop()
}
