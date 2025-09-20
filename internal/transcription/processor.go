package transcription

import (
	"sync"
)

type Processor struct {
	currentTranscript     string
	lastTurnOrder         int
	turnTranscripts       map[int]string
	finalTranscripts      []string  // Accumulate multiple final transcripts
	transcriptMutex       sync.Mutex
	sessionTerminated     chan bool
	sessionActive         bool      // Track if session is actively processing
	resetCount            int       // Track number of resets (for debugging degradation)
	bestPartialTranscript string    // Track best partial transcript as fallback
	bestPartialConfidence float64   // Track confidence of best partial
}

func NewProcessor() *Processor {
	return &Processor{
		lastTurnOrder:    -1,
		turnTranscripts:  make(map[int]string),
		finalTranscripts: make([]string, 0),
		sessionTerminated: make(chan bool, 1),
	}
}

func (p *Processor) ProcessTranscript(transcript string, turnOrder int, isComplete bool, endOfTurn bool, confidence float64) {
	p.transcriptMutex.Lock()
	defer p.transcriptMutex.Unlock()


	// For streaming transcription, AssemblyAI sends progressive updates
	// where each partial transcript contains the complete accumulated text
	if isComplete {
		// Add final transcripts to our collection to handle multiple sessions
		p.finalTranscripts = append(p.finalTranscripts, transcript)


		// Build complete transcript from all final transcripts
		completeText := ""
		for i, finalText := range p.finalTranscripts {
			if i > 0 {
				completeText += " "
			}
			completeText += finalText
		}
		p.currentTranscript = completeText


		// No completion signaling - rely on termination protocol instead
	} else {
		// For partial transcripts, just update current (will be overwritten by final)
		p.currentTranscript = transcript

		// Track best partial transcript as fallback
		if confidence > p.bestPartialConfidence || len(transcript) > len(p.bestPartialTranscript) {
			p.bestPartialTranscript = transcript
			p.bestPartialConfidence = confidence
		}
	}

	// Store for turn tracking compatibility
	p.turnTranscripts[turnOrder] = transcript

	if turnOrder > p.lastTurnOrder {
		p.lastTurnOrder = turnOrder
	}
}

func (p *Processor) GetCurrentTranscript() string {
	p.transcriptMutex.Lock()
	defer p.transcriptMutex.Unlock()
	return p.currentTranscript
}

func (p *Processor) Reset() {
	p.transcriptMutex.Lock()
	defer p.transcriptMutex.Unlock()

	p.currentTranscript = ""
	p.lastTurnOrder = -1
	p.turnTranscripts = make(map[int]string)
	p.finalTranscripts = make([]string, 0)
	p.bestPartialTranscript = ""
	p.bestPartialConfidence = 0.0
	p.sessionActive = true
	p.resetCount++

	// Drain any pending termination signals to prevent state contamination
	select {
	case <-p.sessionTerminated:
	default:
	}
}

func (p *Processor) WaitForTermination() chan bool {
	return p.sessionTerminated
}

func (p *Processor) SignalTermination() {
	select {
	case p.sessionTerminated <- true:
	default:
	}
}

// GetCurrentTranscriptImmediate returns whatever transcript is available right now
func (p *Processor) GetCurrentTranscriptImmediate() string {
	p.transcriptMutex.Lock()
	defer p.transcriptMutex.Unlock()
	return p.currentTranscript
}

// HasAnyTranscript returns true if we have any transcript content (partial or final)
func (p *Processor) HasAnyTranscript() bool {
	p.transcriptMutex.Lock()
	defer p.transcriptMutex.Unlock()
	return len(p.currentTranscript) > 0
}

// GetBestPartialTranscript returns the best partial transcript as fallback
func (p *Processor) GetBestPartialTranscript() (string, float64) {
	p.transcriptMutex.Lock()
	defer p.transcriptMutex.Unlock()
	return p.bestPartialTranscript, p.bestPartialConfidence
}

func (p *Processor) ConsumeTranscript() string {
	p.transcriptMutex.Lock()
	defer p.transcriptMutex.Unlock()

	text := p.currentTranscript

	// Mark session as inactive to prevent contamination
	p.sessionActive = false

	p.currentTranscript = "" // Reset for next recording
	p.lastTurnOrder = -1
	p.turnTranscripts = make(map[int]string)
	p.finalTranscripts = make([]string, 0)
	p.bestPartialTranscript = ""
	p.bestPartialConfidence = 0.0
	return text
}

// ConsumeTranscriptWithFallback returns final transcript or best partial if no final available
func (p *Processor) ConsumeTranscriptWithFallback() (string, bool) {
	p.transcriptMutex.Lock()
	defer p.transcriptMutex.Unlock()

	var text string
	isFinal := len(p.finalTranscripts) > 0

	if isFinal {
		// Use final transcript and add trailing space
		text = p.currentTranscript + " "
	} else if len(p.bestPartialTranscript) > 0 {
		// Use best partial as fallback
		text = p.bestPartialTranscript + " " // Add space for consistency
	} else {
		// No transcript available
		text = ""
	}

	// Mark session as inactive to prevent contamination
	p.sessionActive = false

	// Reset state for next recording
	p.currentTranscript = ""
	p.lastTurnOrder = -1
	p.turnTranscripts = make(map[int]string)
	p.finalTranscripts = make([]string, 0)
	p.bestPartialTranscript = ""
	p.bestPartialConfidence = 0.0

	return text, isFinal
}
