package transcription

import (
	"log"
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

	transcriptType := "partial"
	if isComplete {
		transcriptType = "final"
	}
	log.Printf("[PROC] Processing %s transcript (turn %d): %d chars: \"%s\" | end_of_turn: %v, confidence: %.2f",
		transcriptType, turnOrder, len(transcript), transcript, endOfTurn, confidence)

	// For streaming transcription, AssemblyAI sends progressive updates
	// where each partial transcript contains the complete accumulated text
	if isComplete {
		// Add final transcripts to our collection to handle multiple sessions
		// Append space to ensure proper spacing between sentences
		transcriptWithSpace := transcript + " "
		p.finalTranscripts = append(p.finalTranscripts, transcriptWithSpace)

		log.Printf("[PROC] Added final transcript #%d, total finals: %d", len(p.finalTranscripts), len(p.finalTranscripts))

		// Build complete transcript from all final transcripts
		completeText := ""
		for i, finalText := range p.finalTranscripts {
			if i > 0 {
				completeText += " "
			}
			completeText += finalText
		}
		p.currentTranscript = completeText

		log.Printf("[PROC] Complete accumulated text: %d chars: \"%s\"", len(p.currentTranscript), p.currentTranscript)

		// No completion signaling - rely on termination protocol instead
		log.Printf("[PROC] Final transcript #%d accumulated (total: %d), waiting for termination signal",
			len(p.finalTranscripts), len(p.finalTranscripts))
	} else {
		// For partial transcripts, just update current (will be overwritten by final)
		log.Printf("[PROC] Updated partial transcript: %d chars", len(transcript))
		p.currentTranscript = transcript

		// Track best partial transcript as fallback
		if confidence > p.bestPartialConfidence || len(transcript) > len(p.bestPartialTranscript) {
			p.bestPartialTranscript = transcript
			p.bestPartialConfidence = confidence
			log.Printf("[PROC] New best partial: %d chars, confidence: %.2f", len(transcript), confidence)
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

	log.Printf("[PROC] Resetting processor state")
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
	log.Printf("[PROC] Signaling session termination")
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
	log.Printf("[PROC] ConsumeTranscript: returning %d chars: \"%s\"", len(text), text)
	log.Printf("[PROC] Had %d final transcripts total", len(p.finalTranscripts))

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
		// Use final transcript
		text = p.currentTranscript
		log.Printf("[PROC] ConsumeTranscriptWithFallback: returning final transcript: %d chars: \"%s\"", len(text), text)
		log.Printf("[PROC] Had %d final transcripts total", len(p.finalTranscripts))
	} else if len(p.bestPartialTranscript) > 0 {
		// Use best partial as fallback
		text = p.bestPartialTranscript + " " // Add space for consistency
		log.Printf("[PROC] ConsumeTranscriptWithFallback: using best partial fallback: %d chars: \"%s\" (confidence: %.2f)",
			len(text), text, p.bestPartialConfidence)
	} else {
		// No transcript available
		text = ""
		log.Printf("[PROC] ConsumeTranscriptWithFallback: no transcript available")
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
