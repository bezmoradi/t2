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

// maxFrameBytes caps a single WebSocket audio frame at 800ms of 16kHz PCM16.
// The API accepts frames between 50ms and 1000ms, so buffered audio has to be
// flushed in pieces rather than as one large frame.
const maxFrameBytes = audio.SampleRate * 2 * 800 / 1000

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
	rec *recording // the current recording; nil before the first
}

// recording is one key press's audio and its delivery state. The sender
// goroutine holds it directly, so a sender still finishing a slow write when
// the next recording starts cannot touch the next recording's state.
type recording struct {
	mu      sync.Mutex
	all     []byte // everything captured, append-only
	sent    int    // bytes of all already delivered to the session
	session audioSender
	sendErr error
	closed  bool

	kick chan struct{} // wakes the sender: new audio, a session, or closing
	done chan struct{} // closed when the sender has exited
}

func newAudioSink() *audioSink { return &audioSink{} }

// start begins a new recording and launches its sender.
func (a *audioSink) start() {
	r := &recording{
		kick: make(chan struct{}, 1),
		done: make(chan struct{}),
	}

	a.mu.Lock()
	a.rec = r
	a.mu.Unlock()

	go r.send()
}

func (a *audioSink) current() *recording {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.rec
}

// write is the recorder's audio callback. It only appends.
func (a *audioSink) write(pcm []byte) error {
	r := a.current()
	if r == nil {
		return nil
	}

	r.mu.Lock()
	r.all = append(r.all, pcm...)
	r.mu.Unlock()

	r.wake()
	return nil
}

// attach hands the recording a live session; the sender flushes what was
// buffered and streams from there.
func (a *audioSink) attach(s audioSender) {
	r := a.current()
	if r == nil {
		return
	}

	r.mu.Lock()
	r.session = s
	r.mu.Unlock()

	r.wake()
}

// flush waits until everything captured so far has been sent, the sender has
// failed, or the timeout passes. Called once the recorder has stopped and
// before the session is terminated, so the tail of the recording is not lost.
func (a *audioSink) flush(timeout time.Duration) {
	r := a.current()
	if r == nil {
		return
	}

	deadline := time.Now().Add(timeout)
	for {
		r.mu.Lock()
		drained := r.session == nil || r.sent == len(r.all) || r.sendErr != nil
		r.mu.Unlock()

		if drained || time.Now().After(deadline) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// stop ends the sender. Safe to call more than once. It waits briefly for the
// goroutine; one stuck in a write runs into the session's deadline on its own.
func (a *audioSink) stop() {
	r := a.current()
	if r == nil {
		return
	}

	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	r.wake()

	select {
	case <-r.done:
	case <-time.After(time.Second):
	}
}

// err reports the first send failure of the current recording, if any.
func (a *audioSink) err() error {
	r := a.current()
	if r == nil {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sendErr
}

// audio returns a copy of everything captured in the current recording.
func (a *audioSink) audio() []byte {
	r := a.current()
	if r == nil {
		return nil
	}

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

// send streams all[sent:] to the session in frames of at most maxFrameBytes,
// for as long as the recording lasts.
func (r *recording) send() {
	defer close(r.done)

	for {
		r.mu.Lock()
		for r.session == nil || r.sent == len(r.all) || r.sendErr != nil {
			if r.closed {
				r.mu.Unlock()
				return
			}
			r.mu.Unlock()
			<-r.kick
			r.mu.Lock()
		}
		n := min(len(r.all)-r.sent, maxFrameBytes)
		frame := r.all[r.sent : r.sent+n]
		session := r.session
		r.mu.Unlock()

		// The frame is read without the lock. That is safe because all is
		// append-only for the life of the recording: appends write past the
		// end, and a reallocation copies the bytes rather than moving them.
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
