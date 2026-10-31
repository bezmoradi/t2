package audio

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const (
	SampleRate = 16000
	Frames     = 1024

	// SilenceThreshold is the RMS below which a recording counts as silence.
	// Samples are int16, so full scale is 32767 and this is about -47 dBFS.
	// Shared with the daemon, which applies it to the whole recording.
	SilenceThreshold = 150.0

	// ignoreLeadingChunks excludes the start of a recording from the level
	// measurement. The start beep is played through the speakers immediately
	// before capture opens, and if the microphone hears its tail it would set
	// the peak level on its own and defeat the silence gate entirely.
	ignoreLeadingChunks = 3
)

const (
	// pollInterval is how long the capture loop waits when the device has
	// less than one chunk ready. Every wait happens here, in Go, where Stop
	// can interrupt it; the loop never blocks inside the audio layer.
	pollInterval = 5 * time.Millisecond

	// starvationTimeout is how long the loop tolerates no audio at all, once
	// audio has started, before declaring the device gone. A live device
	// fills a chunk every 64ms.
	starvationTimeout = 2 * time.Second

	// stopTimeout bounds how long Stop waits for the capture loop to notice
	// the stop. The loop only ever appends to the sink, so nothing legitimate
	// holds it beyond one poll interval.
	stopTimeout = 2 * time.Second

	// shutdownTimeout bounds how long Stop then waits for the stream to
	// close, which takes milliseconds. Anything near this is the audio layer
	// hanging inside the OS, and waiting longer only delays the recovery.
	shutdownTimeout = 3 * time.Second
)

// ErrWedged reports that a previous recording's stream never shut down, so
// this process's audio layer is unusable until the process restarts.
var ErrWedged = errors.New("the audio layer is stuck inside " + Backend() + "; t2 has to restart")

// Stream is one open capture from the default microphone: mono int16 at
// SampleRate. OpenStream provides the platform's implementation.
type Stream interface {
	// Available reports how many frames can be read without blocking.
	Available() (int, error)

	// Read fills into with exactly len(into) frames. It is only called once
	// Available has reported at least that many, so it never blocks.
	Read(into []int16) error

	// Overflows reports how many times audio was dropped because it was not
	// read fast enough.
	Overflows() int

	// Close stops the capture and releases the device. It may hang inside
	// the OS; Stop bounds the wait.
	Close() error
}

// framesEnv overrides the chunk size requested from the device, for
// diagnostics: T2_FRAMES_PER_BUFFER=32 on the real binary. A tiny buffer
// multiplies the device's IO cycles and stop handshakes, which is what made
// PortAudio's CoreAudio deadlock reproducible in seconds. Values outside minFrames..maxFrames are
// ignored rather than handed to the audio layer.
const (
	framesEnv = "T2_FRAMES_PER_BUFFER"
	minFrames = 32
	maxFrames = 8192
)

func chunkFrames() int {
	if v, err := strconv.Atoi(os.Getenv(framesEnv)); err == nil && v >= minFrames && v <= maxFrames {
		return v
	}
	return Frames
}

// Outcome describes how a recording ended. Anything other than a clean stop
// means the audio is incomplete, and the caller must not report success.
type Outcome struct {
	// Err is set when capture stopped on its own before Stop was called.
	Err error

	// Overflows counts times the device discarded input before we read it.
	// The data around them is still valid; the audio has gaps.
	Overflows int

	// Chunks is how many audio chunks were captured.
	Chunks int

	// MaxRMS is the level of the loudest chunk after the leading ones, for
	// the silence gate.
	MaxRMS float64

	// Wedged means the stream could not be shut down: the audio layer hung
	// inside the OS. The audio itself was captured in full, but this process
	// cannot record again until it restarts. See Stop.
	Wedged bool
}

// Recorder captures from the default microphone, one recording at a time.
type Recorder struct {
	mu      sync.Mutex
	send    func([]byte)
	current *capture // in-flight recording; nil when idle
}

// NewRecorder returns a recorder that hands every captured chunk to
// audioCallback, on the capture goroutine. The callback must not block.
func NewRecorder(audioCallback func([]byte)) *Recorder {
	return &Recorder{send: audioCallback}
}

func (r *Recorder) IsRecording() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.current != nil
}

// calculateRMS computes the Root Mean Square of int16 audio samples
func calculateRMS(samples []int16) float64 {
	if len(samples) == 0 {
		return 0
	}

	var sum float64
	for _, sample := range samples {
		sum += float64(sample) * float64(sample)
	}
	return math.Sqrt(sum / float64(len(samples)))
}

func (r *Recorder) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.current != nil {
		return nil
	}

	if abandoned.Load() > 0 {
		return ErrWedged
	}

	// The stream is opened fresh for every recording, on whatever the
	// default microphone is right now. There is no device list to refresh:
	// a headset, AirPods or a conferencing app's virtual device that became
	// the default since the last recording is simply what gets opened.
	frames := chunkFrames()
	stream, err := OpenStream(SampleRate, frames)
	if err != nil {
		return err
	}

	c := &capture{
		stream:   stream,
		in:       make([]int16, frames),
		send:     r.send,
		stop:     make(chan struct{}),
		loopDone: make(chan struct{}),
		done:     make(chan struct{}),
	}
	r.current = c
	go c.run()

	return nil
}

// Stop ends capture and reports how the recording actually went.
func (r *Recorder) Stop() Outcome {
	r.mu.Lock()
	c := r.current
	r.current = nil
	r.mu.Unlock()

	if c == nil {
		return Outcome{}
	}

	close(c.stop)

	// Two waits, because the two things that can hold the goroutine up are
	// different in kind. The loop can only be a poll interval away from
	// noticing. Closing the stream takes milliseconds unless the audio layer
	// has hung inside the OS, in which case it never returns, so that wait
	// is short: the sooner it gives up, the sooner the daemon can restart
	// the process, which is the only way out of a wedged audio layer.
	//
	// This watchdog outlived the backend it was written for. PortAudio's
	// CoreAudio backend deadlocked in Pa_StopStream: AUHAL's stop handshake
	// gives the IO thread ~10 IO buffers to acknowledge a stop, and when it
	// was late - under coreaudiod load from other clients starting up -
	// AudioOutputUnitStop called AudioDeviceStop itself while holding the
	// AudioUnit's mutex, needing the IO mutex the IO thread held while it ran
	// PortAudio's IsRunning listener, which needed the AudioUnit's mutex. The
	// old code waited on it with no timeout, so one occurrence left the
	// daemon deaf until someone restarted it. AudioQueue has no such
	// listener, but a stop that hangs inside the OS is still worth catching.
	select {
	case <-c.loopDone:
	case <-time.After(stopTimeout):
		return r.abandon(c)
	}

	select {
	case <-c.done:
	case <-time.After(shutdownTimeout):
		return r.abandon(c)
	}

	return c.outcome()
}

// abandon gives up on a capture stuck inside the audio layer. The capture owns
// its own state and stream, so nothing it does later can reach the next
// recording; Start refuses until it returns, which for a real hang is never.
func (r *Recorder) abandon(c *capture) Outcome {
	abandoned.Add(1)

	go func() {
		<-c.done
		abandoned.Add(-1)
	}()

	out := c.outcome()
	out.Wedged = true
	return out
}

// capture is one recording. The goroutine that reads the device owns it - the
// stream included, which it closes itself on the way out - so that a capture
// Stop had to give up on can never touch the recorder or the next recording.
type capture struct {
	stream Stream
	in     []int16
	send   func([]byte)

	stop     chan struct{} // closed by Stop
	loopDone chan struct{} // closed by run once the loop has exited
	done     chan struct{} // closed by run on exit, once the stream is closed

	mu        sync.Mutex
	maxRMS    float64
	chunks    int
	overflows int
	err       error
}

func (c *capture) run() {
	defer close(c.done)
	defer func() {
		// A panic here used to be swallowed while leaving the recorder marked
		// as recording, so the failure surfaced as a silently truncated
		// transcript.
		if p := recover(); p != nil {
			c.fail(fmt.Errorf("capture panicked: %v", p))
		}
		close(c.loopDone)

		// Only this goroutine ever touches the stream, so Stop never has to
		// close it underneath a read. A close error changes nothing about the
		// audio already captured, so it is not reported as one.
		c.stream.Close()
	}()

	c.loop()
}

func (c *capture) loop() {
	pcm := make([]byte, len(c.in)*2)

	// Armed by the first chunk. Starvation means the device runs no IO
	// cycles at all, which is a different thing from silence and from a slow
	// start: a Bluetooth microphone can pause IO for longer than the timeout
	// while it switches profiles, and every recording on it would otherwise
	// fail.
	var lastAudio time.Time

	for {
		select {
		case <-c.stop:
			return
		default:
		}

		// Only ever read a chunk that is already there, so the loop never
		// blocks inside the audio layer and a device that stops delivering
		// is visible instead of indistinguishable from a quiet room.
		avail, err := c.stream.Available()
		if err != nil {
			c.fail(fmt.Errorf("microphone read failed: %w", err))
			return
		}
		if avail < len(c.in) {
			if !lastAudio.IsZero() && time.Since(lastAudio) > starvationTimeout {
				c.fail(errors.New("microphone stopped delivering audio"))
				return
			}
			time.Sleep(pollInterval)
			continue
		}
		lastAudio = time.Now()

		if err := c.stream.Read(c.in); err != nil {
			c.fail(fmt.Errorf("microphone read failed: %w", err))
			return
		}

		for i, sample := range c.in {
			pcm[i*2] = byte(sample)
			pcm[i*2+1] = byte(sample >> 8)
		}

		rms := calculateRMS(c.in)

		c.mu.Lock()
		c.chunks++
		if c.chunks > ignoreLeadingChunks && rms > c.maxRMS {
			c.maxRMS = rms
		}
		c.overflows = c.stream.Overflows()
		c.mu.Unlock()

		// Every captured chunk is handed on. Withholding audio during leading
		// silence used to be an optimisation for per-audio-second billing, but
		// the API bills wall-clock session time, so it saved nothing and only
		// made the stream discontinuous.
		if c.send != nil {
			c.send(pcm)
		}
	}
}

func (c *capture) fail(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.mu.Unlock()
}

func (c *capture) outcome() Outcome {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Outcome{Err: c.err, Overflows: c.overflows, Chunks: c.chunks, MaxRMS: c.maxRMS}
}

// abandoned counts capture goroutines that never came back from the audio
// layer.
var abandoned atomic.Int32

// Wedged reports whether a capture is stuck inside the audio layer, which
// makes it unusable for this process until the process restarts.
func Wedged() bool { return abandoned.Load() > 0 }
