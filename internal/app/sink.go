package app

import (
	"sync"
	"time"

	"github.com/bezmoradi/t2/internal/audio"
)

type audioSender interface {
	Send(pcm []byte) error
}

const (
	maxFrameMs    = 800
	minFrameMs    = 60
	maxFrameBytes = audio.SampleRate * audio.BytesPerSample * maxFrameMs / 1000
	minFrameBytes = audio.SampleRate * audio.BytesPerSample * minFrameMs / 1000

	flushPollInterval = 10 * time.Millisecond
	senderExitWait    = time.Second
)

type audioSink struct {
	mu     sync.Mutex
	active *recording
}

type recording struct {
	mu           sync.Mutex
	captured     []byte
	sentBytes    int
	session      audioSender
	sendErr      error
	captureEnded bool
	stopped      bool

	wakeSender chan struct{}
	senderDone chan struct{}
}

func newAudioSink() *audioSink { return &audioSink{} }

func (a *audioSink) start() *recording {
	r := &recording{
		wakeSender: make(chan struct{}, 1),
		senderDone: make(chan struct{}),
	}

	a.mu.Lock()
	a.active = r
	a.mu.Unlock()

	go r.sendUntilStopped()
	return r
}

func (a *audioSink) activeRecording() *recording {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.active
}

func (a *audioSink) write(pcm []byte) {
	r := a.activeRecording()
	if r == nil {
		return
	}

	r.mu.Lock()
	r.captured = append(r.captured, pcm...)
	r.mu.Unlock()

	r.wake()
}

func (r *recording) attach(s audioSender) {
	r.mu.Lock()
	r.session = s
	r.mu.Unlock()

	r.wake()
}

func (r *recording) sendRestThenStop(timeout time.Duration) (allSent bool) {
	allSent = r.sendRest(timeout)
	r.stop()
	return allSent
}

func (r *recording) sendRest(timeout time.Duration) (allSent bool) {
	r.mu.Lock()
	r.captureEnded = true
	r.mu.Unlock()
	r.wake()

	deadline := time.Now().Add(timeout)
	for {
		r.mu.Lock()
		everythingSent := r.sentBytes == len(r.captured)
		cannotSend := r.session == nil || r.sendErr != nil
		r.mu.Unlock()

		if everythingSent {
			return true
		}
		if cannotSend || time.Now().After(deadline) {
			return false
		}
		time.Sleep(flushPollInterval)
	}
}

func (r *recording) stop() {
	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()
	r.wake()

	select {
	case <-r.senderDone:
	case <-time.After(senderExitWait):
	}
}

func (r *recording) err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sendErr
}

func (r *recording) audio() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]byte, len(r.captured))
	copy(out, r.captured)
	return out
}

func (r *recording) wake() {
	select {
	case r.wakeSender <- struct{}{}:
	default:
	}
}

func (r *recording) frameReadyLocked() bool {
	if r.session == nil || r.sendErr != nil {
		return false
	}
	backlog := len(r.captured) - r.sentBytes
	return backlog >= minFrameBytes || (r.captureEnded && backlog > 0)
}

func nextFrameSize(backlog int) int {
	n := min(backlog, maxFrameBytes)
	if rest := backlog - n; rest > 0 && rest < minFrameBytes {
		n = backlog - minFrameBytes
	}
	return n
}

func padToMinFrame(frame []byte) []byte {
	if len(frame) >= minFrameBytes {
		return frame
	}
	padded := make([]byte, minFrameBytes)
	copy(padded, frame)
	return padded
}

func (r *recording) sendUntilStopped() {
	defer close(r.senderDone)

	for {
		r.mu.Lock()
		for !r.stopped && !r.frameReadyLocked() {
			r.mu.Unlock()
			<-r.wakeSender
			r.mu.Lock()
		}
		if r.stopped {
			r.mu.Unlock()
			return
		}
		n := nextFrameSize(len(r.captured) - r.sentBytes)
		frame := r.captured[r.sentBytes : r.sentBytes+n]
		session := r.session
		r.mu.Unlock()

		err := session.Send(padToMinFrame(frame))

		r.mu.Lock()
		if err != nil {
			r.sendErr = err
		} else {
			r.sentBytes += n
		}
		r.mu.Unlock()
	}
}
