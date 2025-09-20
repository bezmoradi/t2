package transcription

import (
	"log"
	"sync"
	"time"
)

type Processor struct {
	currentTranscript     string
	lastTurnOrder         int
	turnTranscripts       map[int]string
	finalTranscripts      []string  // Accumulate multiple final transcripts
	lastFinalTime         time.Time // Track when last final transcript was received
	transcriptMutex       sync.Mutex
	transcriptionComplete chan bool
	sessionTerminated     chan bool
	sessionActive         bool      // Track if session is actively processing
	resetCount            int       // Track number of resets (for debugging degradation)
}

func NewProcessor() *Processor {
	return &Processor{
		lastTurnOrder:         -1,
		turnTranscripts:       make(map[int]string),
		finalTranscripts:      make([]string, 0),
		transcriptionComplete: make(chan bool, 1),
		sessionTerminated:     make(chan bool, 1),
	}
}

func (p *Processor) ProcessTranscript(transcript string, turnOrder int, isComplete bool) {
	p.transcriptMutex.Lock()
	defer p.transcriptMutex.Unlock()

	transcriptType := "partial"
	if isComplete {
		transcriptType = "final"
	}
	log.Printf("[PROC] Processing %s transcript (turn %d): %d chars: \"%s\"", transcriptType, turnOrder, len(transcript), transcript)

	// For streaming transcription, AssemblyAI sends progressive updates
	// where each partial transcript contains the complete accumulated text
	if isComplete {
		// Add final transcripts to our collection to handle multiple sessions
		// Append space to ensure proper spacing between sentences
		transcriptWithSpace := transcript + " "
		p.finalTranscripts = append(p.finalTranscripts, transcriptWithSpace)
		p.lastFinalTime = time.Now() // Track when we received this final

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

		// Only signal completion on the first final transcript
		if len(p.finalTranscripts) == 1 {
			log.Printf("[PROC] Signaling first completion")
			select {
			case p.transcriptionComplete <- true:
			default:
			}
		}
	} else {
		// For partial transcripts, just update current (will be overwritten by final)
		log.Printf("[PROC] Updated partial transcript: %d chars", len(transcript))
		p.currentTranscript = transcript
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
	p.sessionActive = true
	p.resetCount++

	// Drain any pending channels to prevent blocking and state contamination
	select {
	case <-p.transcriptionComplete:
	default:
	}
	select {
	case <-p.sessionTerminated:
	default:
	}
}

func (p *Processor) WaitForComplete() chan bool {
	return p.transcriptionComplete
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

func (p *Processor) IsLikelyComplete() bool {
	p.transcriptMutex.Lock()
	defer p.transcriptMutex.Unlock()

	// If we have no final transcripts, we're not complete
	if len(p.finalTranscripts) == 0 {
		log.Printf("[PROC] IsLikelyComplete: false (no finals)")
		return false
	}

	// If we haven't received a final transcript in the last 1.5 seconds,
	// we're likely done (AssemblyAI can have significant gaps between finals)
	timeSinceLastFinal := time.Since(p.lastFinalTime)
	isComplete := timeSinceLastFinal > 1500*time.Millisecond
	log.Printf("[PROC] IsLikelyComplete: %v (time since last final: %v)", isComplete, timeSinceLastFinal)
	return isComplete
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
	return text
}
