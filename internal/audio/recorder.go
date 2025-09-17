package audio

import (
	"log"
	"sync"
	"time"

	"github.com/gordonklaus/portaudio"
)

const (
	SampleRate = 16000
	Frames     = 1024
)

type Recorder struct {
	recording      bool
	stream         *portaudio.Stream
	recordingMutex sync.Mutex
	audioCallback  func([]byte) error
}

func NewRecorder(audioCallback func([]byte) error) *Recorder {
	return &Recorder{
		audioCallback: audioCallback,
	}
}

func (r *Recorder) IsRecording() bool {
	r.recordingMutex.Lock()
	defer r.recordingMutex.Unlock()
	return r.recording
}

func (r *Recorder) Start() error {
	r.recordingMutex.Lock()
	defer r.recordingMutex.Unlock()

	if r.recording {
		return nil
	}

	r.recording = true

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
		return err
	}

	// Start streaming audio in a goroutine
	go r.audioStreamLoop(in)

	return nil
}

func (r *Recorder) Stop() {
	r.recordingMutex.Lock()
	defer r.recordingMutex.Unlock()

	if !r.recording {
		return
	}

	r.recording = false

	// Give audio goroutine time to finish
	time.Sleep(50 * time.Millisecond)

	// Stop and close the stream
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
	}()

	for {
		r.recordingMutex.Lock()
		isRecording := r.recording
		currentStream := r.stream
		r.recordingMutex.Unlock()

		if !isRecording || currentStream == nil {
			break
		}

		if err := currentStream.Read(); err != nil {
			r.recordingMutex.Lock()
			if r.recording { // Only log if we're still supposed to be recording
				log.Printf("Error reading from stream: %v", err)
			}
			r.recordingMutex.Unlock()
			break
		}

		// Convert int32 to PCM16 bytes for AssemblyAI (little-endian)
		pcmBytes := make([]byte, len(in)*2) // 2 bytes per int16
		for i, sample := range in {
			// Convert int32 to int16 (PCM16)
			sample16 := int16(sample >> 16)
			pcmBytes[i*2] = byte(sample16)        // Low byte
			pcmBytes[i*2+1] = byte(sample16 >> 8) // High byte
		}

		// Always send audio during recording to avoid losing quiet speech
		// The threshold was causing loss of quiet speech at the beginning
		if r.audioCallback != nil {
			// Send audio chunk to callback
			if err := r.audioCallback(pcmBytes); err != nil {
				if r.recording { // Only log if we're still supposed to be recording
					log.Printf("Error in audio callback: %v", err)
				}
				// Continue trying to send, don't break the loop
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
