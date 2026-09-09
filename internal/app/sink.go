package app

import (
	"sync"

	"github.com/bezmoradi/t2/internal/audio"
	"github.com/bezmoradi/t2/internal/transcription"
)

// maxFrameBytes caps a single WebSocket audio frame at 800ms of 16kHz PCM16.
// The API accepts frames between 50ms and 1000ms, so buffered audio has to be
// flushed in pieces rather than as one large frame.
const maxFrameBytes = audio.SampleRate * 2 * 800 / 1000

// audioSink sits between the recorder and the network.
//
// Capture starts immediately on key-down and the session is dialled in
// parallel, so audio arrives here before there is anywhere to send it. It is
// buffered until the session is ready, then flushed and streamed live. That is
// what stops the TLS handshake from eating the first word of every recording.
//
// It also keeps the full recording in memory - about 2MB per minute - so that
// any failure downstream can still be turned into a WAV on disk instead of
// losing what the user said.
type audioSink struct {
	mu      sync.Mutex
	pending []byte // captured but not yet sent
	all     []byte // everything captured this recording
	session *transcription.Session
	sendErr error
}

func newAudioSink() *audioSink { return &audioSink{} }

// reset clears state for a new recording.
func (a *audioSink) reset() {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.pending = a.pending[:0]
	a.all = a.all[:0]
	a.session = nil
	a.sendErr = nil
}

// write is the recorder's audio callback.
func (a *audioSink) write(pcm []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.all = append(a.all, pcm...)

	if a.session == nil {
		a.pending = append(a.pending, pcm...)
		return nil
	}

	// Once sending has failed, keep capturing but stop hammering a dead socket
	// with one error per chunk. The recording is still saved locally.
	if a.sendErr != nil {
		return nil
	}

	if err := a.session.Send(pcm); err != nil {
		a.sendErr = err
	}

	return nil
}

// attach hands the sink a live session and flushes whatever was buffered.
func (a *audioSink) attach(s *transcription.Session) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.session = s

	for len(a.pending) > 0 && a.sendErr == nil {
		n := min(len(a.pending), maxFrameBytes)
		if err := s.Send(a.pending[:n]); err != nil {
			a.sendErr = err
			break
		}
		a.pending = a.pending[n:]
	}

	a.pending = a.pending[:0]
}

// err reports the first send failure, if any.
func (a *audioSink) err() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sendErr
}

// audio returns a copy of everything captured this recording.
func (a *audioSink) audio() []byte {
	a.mu.Lock()
	defer a.mu.Unlock()

	out := make([]byte, len(a.all))
	copy(out, a.all)
	return out
}
