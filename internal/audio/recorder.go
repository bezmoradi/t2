package audio

import (
	"encoding/binary"
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
	SampleRate       = 16000
	Frames           = 1024
	SilenceThreshold = 150.0

	BytesPerSample      = 2
	startBeepEchoChunks = 3
)

const (
	pollInterval       = 5 * time.Millisecond
	starvationTimeout  = 2 * time.Second
	loopStopTimeout    = 2 * time.Second
	streamCloseTimeout = 3 * time.Second
)

var ErrWedged = errors.New("the audio layer is stuck inside " + Backend() + "; t2 has to restart")

type Stream interface {
	Available() (int, error)
	Read(into []int16) error
	Overflows() int
	Close() error
}

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

type Outcome struct {
	Err       error
	Overflows int
	Chunks    int
	MaxRMS    float64
	Wedged    bool
}

type Recorder struct {
	mu      sync.Mutex
	send    func([]byte)
	current *capture
}

func NewRecorder(audioCallback func([]byte)) *Recorder {
	return &Recorder{send: audioCallback}
}

func (r *Recorder) IsRecording() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.current != nil
}

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

	if Wedged() {
		return ErrWedged
	}

	frames := chunkFrames()
	stream, err := OpenStream(SampleRate, frames)
	if err != nil {
		return err
	}

	c := &capture{
		stream:       stream,
		in:           make([]int16, frames),
		send:         r.send,
		stop:         make(chan struct{}),
		loopExited:   make(chan struct{}),
		streamClosed: make(chan struct{}),
	}
	r.current = c
	go c.run()

	return nil
}

func (r *Recorder) Stop() Outcome {
	r.mu.Lock()
	c := r.current
	r.current = nil
	r.mu.Unlock()

	if c == nil {
		return Outcome{}
	}

	close(c.stop)

	select {
	case <-c.loopExited:
	case <-time.After(loopStopTimeout):
		return r.abandon(c)
	}

	select {
	case <-c.streamClosed:
	case <-time.After(streamCloseTimeout):
		return r.abandon(c)
	}

	return c.outcome()
}

func (r *Recorder) abandon(c *capture) Outcome {
	abandonedCaptures.Add(1)

	go func() {
		<-c.streamClosed
		abandonedCaptures.Add(-1)
	}()

	out := c.outcome()
	out.Wedged = true
	return out
}

type capture struct {
	stream Stream
	in     []int16
	send   func([]byte)

	stop         chan struct{}
	loopExited   chan struct{}
	streamClosed chan struct{}

	mu        sync.Mutex
	maxRMS    float64
	chunks    int
	overflows int
	err       error
}

func (c *capture) run() {
	defer close(c.streamClosed)
	defer func() {
		if p := recover(); p != nil {
			c.fail(fmt.Errorf("capture panicked: %v", p))
		}
		close(c.loopExited)
		_ = c.stream.Close()
	}()

	c.loop()
}

func (c *capture) loop() {
	pcm := make([]byte, len(c.in)*BytesPerSample)
	var lastChunkAt time.Time

	for {
		select {
		case <-c.stop:
			return
		default:
		}

		avail, err := c.stream.Available()
		if err != nil {
			c.fail(fmt.Errorf("microphone read failed: %w", err))
			return
		}
		if avail < len(c.in) {
			audioStarted := !lastChunkAt.IsZero()
			if audioStarted && time.Since(lastChunkAt) > starvationTimeout {
				c.fail(errors.New("microphone stopped delivering audio"))
				return
			}
			time.Sleep(pollInterval)
			continue
		}
		lastChunkAt = time.Now()

		if err := c.stream.Read(c.in); err != nil {
			c.fail(fmt.Errorf("microphone read failed: %w", err))
			return
		}

		for i, sample := range c.in {
			binary.LittleEndian.PutUint16(pcm[i*BytesPerSample:], uint16(sample))
		}

		rms := calculateRMS(c.in)

		c.mu.Lock()
		c.chunks++
		pastStartBeep := c.chunks > startBeepEchoChunks
		if pastStartBeep && rms > c.maxRMS {
			c.maxRMS = rms
		}
		c.overflows = c.stream.Overflows()
		c.mu.Unlock()

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

var abandonedCaptures atomic.Int32

func Wedged() bool { return abandonedCaptures.Load() > 0 }
