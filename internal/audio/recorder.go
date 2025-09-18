package audio

import (
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/gordonklaus/portaudio"
)

const (
	SampleRate = 16000
	Frames     = 1024
)

type Recorder struct {
	recording        bool
	stream           *portaudio.Stream
	recordingMutex   sync.Mutex
	audioCallback    func([]byte) error
	stopChan         chan struct{}
	streamWg         sync.WaitGroup
	maxRMS           float64
	silenceThreshold float64
}

func NewRecorder(audioCallback func([]byte) error) *Recorder {
	return &Recorder{
		audioCallback:    audioCallback,
		stopChan:         make(chan struct{}),
		silenceThreshold: 250.0, // Balanced threshold for int16 samples - allows quiet speech
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

	r.recording = true

	// Reset audio level tracking for new session
	r.maxRMS = 0.0

	// Create new stop channel for this session
	r.stopChan = make(chan struct{})

	// Setup audio buffer for streaming (PCM16 format for AssemblyAI)
	in := make([]int32, Frames)

	// Open PortAudio stream
	var err error
	r.stream, err = portaudio.OpenDefaultStream(1, 0, SampleRate, len(in), in)
	if err != nil {
		log.Printf("Error opening PortAudio stream: %v", err)
		r.recording = false
		return err
	}

	// Start the stream
	if err := r.stream.Start(); err != nil {
		log.Printf("Error starting PortAudio stream: %v", err)
		r.recording = false
		r.stream.Close()
		r.stream = nil
		return err
	}

	// Start streaming audio in a goroutine with proper synchronization
	r.streamWg.Add(1)
	go r.audioStreamLoop(in)

	return nil
}

func (r *Recorder) Stop() {
	r.recordingMutex.Lock()

	if !r.recording {
		r.recordingMutex.Unlock()
		return
	}

	r.recording = false

	// Signal the audio goroutine to stop
	close(r.stopChan)

	r.recordingMutex.Unlock()

	// Wait for the audio goroutine to finish properly
	r.streamWg.Wait()

	// Now safely clean up the stream
	r.recordingMutex.Lock()
	defer r.recordingMutex.Unlock()

	if r.stream != nil {
		r.stream.Stop()
		r.stream.Close()
		r.stream = nil
	}
}

func (r *Recorder) audioStreamLoop(in []int32) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Audio streaming goroutine recovered from panic: %v", r)
		}
		r.streamWg.Done() // Signal that the goroutine has finished
	}()

	for {
		// Check if we should stop using the stop channel
		select {
		case <-r.stopChan:
			return
		default:
		}

		// Get current stream state safely
		r.recordingMutex.Lock()
		isRecording := r.recording
		currentStream := r.stream
		r.recordingMutex.Unlock()

		// Exit if not recording or stream is nil
		if !isRecording || currentStream == nil {
			return
		}

		// Perform the stream read with proper error handling
		if err := currentStream.Read(); err != nil {
			// Check if we're still supposed to be recording before logging
			select {
			case <-r.stopChan:
				// Stop was called, this error is expected
				return
			default:
				r.recordingMutex.Lock()
				stillRecording := r.recording
				r.recordingMutex.Unlock()

				if stillRecording {
					log.Printf("Error reading from stream: %v", err)
				}
				return
			}
		}

		// Convert int32 to PCM16 bytes for AssemblyAI (little-endian)
		pcmBytes := make([]byte, len(in)*2) // 2 bytes per int16
		samples16 := make([]int16, len(in))  // For RMS calculation

		for i, sample := range in {
			// Convert int32 to int16 (PCM16)
			sample16 := int16(sample >> 16)
			samples16[i] = sample16
			pcmBytes[i*2] = byte(sample16)        // Low byte
			pcmBytes[i*2+1] = byte(sample16 >> 8) // High byte
		}

		// Calculate RMS for this chunk and update maximum
		chunkRMS := calculateRMS(samples16)
		r.recordingMutex.Lock()
		if chunkRMS > r.maxRMS {
			r.maxRMS = chunkRMS
		}
		r.recordingMutex.Unlock()

		// Check again if we should stop before sending audio
		select {
		case <-r.stopChan:
			return
		default:
		}

		// Always send audio during recording to avoid losing quiet speech
		// The threshold was causing loss of quiet speech at the beginning
		if r.audioCallback != nil {
			// Send audio chunk to callback
			if err := r.audioCallback(pcmBytes); err != nil {
				// Check if stop was called before logging error
				select {
				case <-r.stopChan:
					return
				default:
					r.recordingMutex.Lock()
					stillRecording := r.recording
					r.recordingMutex.Unlock()

					if stillRecording {
						// Check if it's a WebSocket close error - if so, stop sending
						errStr := err.Error()
						if strings.Contains(errStr, "websocket: close sent") ||
							strings.Contains(errStr, "use of closed network connection") ||
							strings.Contains(errStr, "connection reset by peer") {
							// WebSocket is closed, stop the audio stream
							return
						}
						log.Printf("Error in audio callback: %v", err)
					}
					// Continue trying to send, don't break the loop (unless WebSocket is closed)
				}
			}
		}

		// Reduce delay to improve real-time performance
		time.Sleep(10 * time.Millisecond)
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
