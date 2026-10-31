package app

import (
	"sync"
	"time"

	"github.com/bezmoradi/t2/internal/audio"
)

// audioSender is what the sink streams to: a transcription.Session in the
// daemon, a fake in tests.
type audioSender interface {
	Send(pcm []byte) error
}

// The API accepts audio frames between 50ms and 1000ms of 16kHz PCM16, and
// closes the session (3007) on anything outside that.
//
// maxFrameBytes caps a frame at 800ms, so buffered audio is flushed in pieces
// rather than as one large frame. minFrameBytes is 60ms, a margin above the
// floor: live audio is held back until there is at least that much, and the
// last piece of a recording is padded up to it with silence. Without the
// floor, an 800ms frame taken from a backlog left whatever remained - as
// little as 32ms - to go out on its own, and the T2_FRAMES_PER_BUFFER=32
// diagnostic sent every frame at 2ms.
const (
	maxFrameBytes = audio.SampleRate * 2 * 800 / 1000
	minFrameBytes = audio.SampleRate * 2 * 60 / 1000
)

// audioSink sits between the recorder and the network.
//
// Capture starts immediately on key-down and the session is dialled in
// parallel, so audio arrives here before there is anywhere to send it. It is
// kept until the session is ready, then streamed by a sender goroutine that
// runs for the length of the recording. That is what stops the TLS handshake
// from eating the first word of every recording.
//
// The sender is a separate goroutine on purpose. Sending used to happen on the
// capture goroutine, inside the recorder's read loop, so a stalled WebSocket
// write held the capture for up to its 5s deadline: PortAudio's 256ms ring
// buffer overflowed and lost audio, and a Stop during the stall looked exactly
// like the CoreAudio deadlock and restarted the process. Now the capture
// goroutine only appends, which never blocks on anything but a mutex.
//
// The full recording stays in memory - about 2MB per minute - so that any
// failure downstream can still be turned into a WAV on disk instead of losing
// what the user said. What is not yet sent is simply all[sent:], which also
// replaces the second buffer the old design kept re-slicing and re-allocating.
type audioSink struct {
	mu  sync.Mutex
	rec *recording // the recording the microphone is feeding; nil before the first
}

// recording is one key press's audio and its delivery state. Everything after
// the capture - attaching the session, flushing, stopping - goes through the
// recording itself rather than the sink, because the next recording can start
// while this one is still being finished.
type recording struct {
	mu      sync.Mutex
	all     []byte // everything captured, append-only
	sent    int    // bytes of all already delivered to the session
	session audioSender
	sendErr error
	final   bool // capture is over: send the tail even if it is short
	closed  bool

	kick chan struct{} // wakes the sender: new audio, a session, final, or closing
	done chan struct{} // closed when the sender has exited
}

func newAudioSink() *audioSink { return &audioSink{} }

// start begins a new recording, makes it the one the microphone feeds, and
// launches its sender.
func (a *audioSink) start() *recording {
	r := &recording{
		kick: make(chan struct{}, 1),
		done: make(chan struct{}),
	}

	a.mu.Lock()
	a.rec = r
	a.mu.Unlock()

	go r.send()
	return r
}

func (a *audioSink) current() *recording {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.rec
}

// write is the recorder's audio callback. It only appends.
func (a *audioSink) write(pcm []byte) {
	r := a.current()
	if r == nil {
		return
	}

	r.mu.Lock()
	r.all = append(r.all, pcm...)
	r.mu.Unlock()

	r.wake()
}

// attach hands the recording a live session; the sender flushes what was
// buffered and streams from there.
func (r *recording) attach(s audioSender) {
	r.mu.Lock()
	r.session = s
	r.mu.Unlock()

	r.wake()
}

// flush waits until everything captured has been sent, the sender has failed,
// or the timeout passes, and reports whether everything was sent. Called once
// the recorder has stopped and before the session is terminated, so the tail
// of the recording is not lost - and when it is, the caller knows.
func (r *recording) flush(timeout time.Duration) bool {
	r.mu.Lock()
	r.final = true
	r.mu.Unlock()
	r.wake()

	deadline := time.Now().Add(timeout)
	for {
		r.mu.Lock()
		sent := r.sent == len(r.all)
		gaveUp := r.session == nil || r.sendErr != nil
		r.mu.Unlock()

		if sent {
			return true
		}
		if gaveUp || time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// stop ends the sender. Safe to call more than once. No frame is started
// after stop returns - audio after Terminate is undefined on the server - so
// whatever flush did not deliver is dropped here, and flush's result is how
// the caller finds out. It waits briefly for a write already in progress; one
// stuck runs into the session's deadline on its own.
func (r *recording) stop() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	r.wake()

	select {
	case <-r.done:
	case <-time.After(time.Second):
	}
}

// err reports the first send failure, if any.
func (r *recording) err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sendErr
}

// audio returns a copy of everything captured.
func (r *recording) audio() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]byte, len(r.all))
	copy(out, r.all)
	return out
}

func (r *recording) wake() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// ready reports whether there is a frame to send. Called with mu held.
func (r *recording) ready() bool {
	if r.session == nil || r.sendErr != nil {
		return false
	}
	backlog := len(r.all) - r.sent
	return backlog >= minFrameBytes || (r.final && backlog > 0)
}

// frameSize is how much of a backlog goes into the next frame: at most
// maxFrameBytes, but never so much that what remains is a sliver below
// minFrameBytes.
func frameSize(backlog int) int {
	n := min(backlog, maxFrameBytes)
	if rest := backlog - n; rest > 0 && rest < minFrameBytes {
		n = backlog - minFrameBytes
	}
	return n
}

// send streams all[sent:] to the session for as long as the recording lasts.
func (r *recording) send() {
	defer close(r.done)

	for {
		r.mu.Lock()
		for !r.closed && !r.ready() {
			r.mu.Unlock()
			<-r.kick
			r.mu.Lock()
		}
		if r.closed {
			r.mu.Unlock()
			return
		}
		n := frameSize(len(r.all) - r.sent)
		frame := r.all[r.sent : r.sent+n]
		session := r.session
		r.mu.Unlock()

		// The frame is read without the lock. That is safe because all is
		// append-only for the life of the recording: appends write past the
		// end, and a reallocation copies the bytes rather than moving them.
		// Only the final tail is ever short, and it is padded with silence
		// in a copy rather than in place.
		if n < minFrameBytes {
			padded := make([]byte, minFrameBytes)
			copy(padded, frame)
			frame = padded
		}
		err := session.Send(frame)

		r.mu.Lock()
		if err != nil {
			// Keep capturing; stop hammering a dead socket. The recording is
			// still saved locally.
			r.sendErr = err
		} else {
			r.sent += n
		}
		r.mu.Unlock()
	}
}
