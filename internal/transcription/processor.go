package transcription

import (
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

	// For streaming transcription, AssemblyAI sends progressive updates
	// where each partial transcript contains the complete accumulated text
	if isComplete {
		// Add final transcripts to our collection to handle multiple sessions
		// Append space to ensure proper spacing between sentences
		transcriptWithSpace := transcript + " "
		p.finalTranscripts = append(p.finalTranscripts, transcriptWithSpace)
		p.lastFinalTime = time.Now() // Track when we received this final

		// Build complete transcript from all final transcripts
		completeText := ""
		for i, finalText := range p.finalTranscripts {
			if i > 0 {
				completeText += " "
			}
			completeText += finalText
		}
		p.currentTranscript = completeText

		// Only signal completion on the first final transcript
		if len(p.finalTranscripts) == 1 {
			select {
			case p.transcriptionComplete <- true:
			default:
			}
		}
	} else {
		// For partial transcripts, just update current (will be overwritten by final)
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

	p.currentTranscript = ""
	p.lastTurnOrder = -1
	p.turnTranscripts = make(map[int]string)
	p.finalTranscripts = make([]string, 0)
}

func (p *Processor) WaitForComplete() chan bool {
	return p.transcriptionComplete
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

func (p *Processor) IsLikelyComplete() bool {
	p.transcriptMutex.Lock()
	defer p.transcriptMutex.Unlock()

	// If we have no final transcripts, we're not complete
	if len(p.finalTranscripts) == 0 {
		return false
	}

	// If we haven't received a final transcript in the last 1.5 seconds,
	// we're likely done (AssemblyAI can have significant gaps between finals)
	return time.Since(p.lastFinalTime) > 1500*time.Millisecond
}

func (p *Processor) ConsumeTranscript() string {
	p.transcriptMutex.Lock()
	defer p.transcriptMutex.Unlock()

	text := p.currentTranscript
	p.currentTranscript = "" // Reset for next recording
	p.lastTurnOrder = -1
	p.turnTranscripts = make(map[int]string)
	p.finalTranscripts = make([]string, 0)
	return text
}
