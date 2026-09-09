package audio

import (
	"errors"
	"fmt"
	"math"
	"sync"

	"github.com/gordonklaus/portaudio"
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

// Outcome describes how a recording ended. Anything other than a clean stop
// means the audio is incomplete, and the caller must not report success.
type Outcome struct {
	// Err is set when capture stopped on its own before Stop was called.
	Err error

	// Overflows counts chunks where PortAudio discarded input before we read
	// it. The data in those chunks is still valid; the audio has a gap.
	Overflows int

	// Chunks is how many audio chunks were captured.
	Chunks int
}

// Incomplete reports whether the recording ended early or lost audio.
func (o Outcome) Incomplete() bool { return o.Err != nil || o.Overflows > 0 }

type Recorder struct {
	recording      bool
	stream         *portaudio.Stream
	recordingMutex sync.Mutex
	audioCallback  func([]byte) error
	stopChan       chan struct{}
	streamWg       sync.WaitGroup

	maxRMS    float64
	chunks    int
	overflows int
	exitErr   error
}

func NewRecorder(audioCallback func([]byte) error) *Recorder {
	return &Recorder{
		audioCallback: audioCallback,
		stopChan:      make(chan struct{}),
	}
}

func (r *Recorder) IsRecording() bool {
	r.recordingMutex.Lock()
	defer r.recordingMutex.Unlock()
	return r.recording
}

func (r *Recorder) GetMaxRMS() float64 {
	r.recordingMutex.Lock()
	defer r.recordingMutex.Unlock()
	return r.maxRMS
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
	r.recordingMutex.Lock()
	defer r.recordingMutex.Unlock()

	if r.recording {
		return nil
	}

	// Reset per-recording state.
	r.maxRMS = 0.0
	r.chunks = 0
	r.overflows = 0
	r.exitErr = nil
	r.stopChan = make(chan struct{})

	// Capture straight into int16: it is what AssemblyAI wants on the wire and
	// what the RMS maths needs, so nothing has to be converted per chunk.
	in := make([]int16, Frames)

	stream, err := portaudio.OpenDefaultStream(1, 0, SampleRate, len(in), in)
	if err != nil {
		return fmt.Errorf("opening microphone: %w", err)
	}

	if err := stream.Start(); err != nil {
		stream.Close()
		return fmt.Errorf("starting microphone: %w", err)
	}

	r.stream = stream
	r.recording = true

	r.streamWg.Add(1)
	go r.audioStreamLoop(in)

	return nil
}

// Stop ends capture and reports how the recording actually went.
func (r *Recorder) Stop() Outcome {
	r.recordingMutex.Lock()

	if !r.recording {
		r.recordingMutex.Unlock()
		return Outcome{}
	}

	r.recording = false
	close(r.stopChan)
	r.recordingMutex.Unlock()

	// Wait for the audio goroutine before touching the stream.
	r.streamWg.Wait()

	r.recordingMutex.Lock()
	defer r.recordingMutex.Unlock()

	if r.stream != nil {
		r.stream.Stop()
		r.stream.Close()
		r.stream = nil
	}

	return Outcome{Err: r.exitErr, Overflows: r.overflows, Chunks: r.chunks}
}

func (r *Recorder) audioStreamLoop(in []int16) {
	defer func() {
		// A panic here used to be swallowed while leaving the recorder marked as
		// recording, so the failure surfaced as a silently truncated transcript.
		if p := recover(); p != nil {
			r.recordingMutex.Lock()
			r.exitErr = fmt.Errorf("capture panicked: %v", p)
			r.recordingMutex.Unlock()
		}
		r.streamWg.Done()
	}()

	pcm := make([]byte, len(in)*2)

	for {
		select {
		case <-r.stopChan:
			return
		default:
		}

		// Read blocks until a full buffer is available, which is the loop's
		// pacing. There is no sleep here on purpose: sleeping only eats the
		// headroom in PortAudio's ring buffer and causes the overflows below.
		if err := r.stream.Read(); err != nil {
			// paInputOverflowed is reported alongside a full, valid buffer: it
			// means input was discarded before this call, not that the read
			// failed. Treating it as fatal killed capture mid-sentence and the
			// tool still reported success.
			if errors.Is(err, portaudio.InputOverflowed) {
				r.recordingMutex.Lock()
				r.overflows++
				r.recordingMutex.Unlock()
			} else {
				select {
				case <-r.stopChan:
					// Expected: Stop closed the stream out from under the read.
				default:
					r.recordingMutex.Lock()
					r.exitErr = fmt.Errorf("microphone read failed: %w", err)
					r.recordingMutex.Unlock()
				}
				return
			}
		}

		for i, sample := range in {
			pcm[i*2] = byte(sample)
			pcm[i*2+1] = byte(sample >> 8)
		}

		chunkRMS := calculateRMS(in)

		r.recordingMutex.Lock()
		r.chunks++
		if r.chunks > ignoreLeadingChunks && chunkRMS > r.maxRMS {
			r.maxRMS = chunkRMS
		}
		r.recordingMutex.Unlock()

		if r.audioCallback == nil {
			continue
		}

		// Every captured chunk is sent. Withholding audio during leading silence
		// used to be an optimisation for per-audio-second billing, but the API
		// bills wall-clock session time, so it saved nothing and only made the
		// stream discontinuous.
		if err := r.audioCallback(pcm); err != nil {
			select {
			case <-r.stopChan:
			default:
				r.recordingMutex.Lock()
				r.exitErr = err
				r.recordingMutex.Unlock()
			}
			return
		}
	}
}

// Initialize initializes PortAudio - should be called at application startup
func Initialize() error {
	return portaudio.Initialize()
}

// Terminate terminates PortAudio - should be called at application shutdown
func Terminate() {
	portaudio.Terminate()
}
