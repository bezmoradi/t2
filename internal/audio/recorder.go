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

// SpeechState represents the current state of speech detection
type SpeechState int

const (
	WaitingForSpeech SpeechState = iota // Waiting for initial speech - aggressive silence detection
	SpeechDetected                      // Speech has been detected - disable silence cutoff
)

type Recorder struct {
	recording        bool
	stream           *portaudio.Stream
	recordingMutex   sync.Mutex
	audioCallback    func([]byte) error
	silenceCallback  func()              // Called when silence is detected
	stopChan         chan struct{}
	streamWg         sync.WaitGroup
	maxRMS           float64
	silenceThreshold float64
	silenceChunks    int                 // Count of consecutive silent chunks
	maxSilenceChunks int                 // Max silent chunks before triggering callback
	speechState      SpeechState         // Track current speech detection state
	prolongedSilence bool                // Flag to track if we've had prolonged silence without speech
}

func NewRecorder(audioCallback func([]byte) error) *Recorder {
	return &Recorder{
		audioCallback:    audioCallback,
		stopChan:         make(chan struct{}),
		silenceThreshold: 150.0, // Threshold for silence detection (lowered to match daemon)
		maxSilenceChunks: 20,     // ~500ms of silence at 40ms chunks (20*25ms per chunk)
	}
}

// SetSilenceCallback sets the callback function for silence detection
func (r *Recorder) SetSilenceCallback(callback func()) {
	r.recordingMutex.Lock()
	defer r.recordingMutex.Unlock()
	r.silenceCallback = callback
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

func (r *Recorder) HasProlongedSilence() bool {
	r.recordingMutex.Lock()
	defer r.recordingMutex.Unlock()
	return r.prolongedSilence
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

	// Reset silence detection for new session
	r.silenceChunks = 0
	r.speechState = WaitingForSpeech
	r.prolongedSilence = false

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

		// Real-time silence detection
		isSilent := chunkRMS < r.silenceThreshold
		if isSilent {
			r.silenceChunks++
		} else {
			// Speech detected - reset silence counter and update state
			r.silenceChunks = 0

			// Transition from WaitingForSpeech to SpeechDetected
			if r.speechState == WaitingForSpeech {
				r.speechState = SpeechDetected
			}
		}

		// Mark prolonged silence but don't stop recording yet
		// Let user decide when to release keys
		if r.speechState == WaitingForSpeech && r.silenceChunks >= r.maxSilenceChunks {
			if !r.prolongedSilence {
				r.prolongedSilence = true
			}
		}
		r.recordingMutex.Unlock()

		// Check again if we should stop before sending audio
		select {
		case <-r.stopChan:
			return
		default:
		}

		// Only send audio to API if speech has been detected or we haven't hit prolonged silence yet
		// This avoids unnecessary API calls during prolonged silence periods
		r.recordingMutex.Lock()
		shouldSendAudio := r.speechState == SpeechDetected || !r.prolongedSilence
		r.recordingMutex.Unlock()

		if r.audioCallback != nil && shouldSendAudio {
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
