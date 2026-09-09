// Package transcription streams PCM audio to AssemblyAI's realtime API.
//
// The unit here is a Session: one key press, one WebSocket, one transcript.
// Nothing is shared between recordings. That is deliberate - the previous
// design reused a single client across connections, which let a late message
// from a finished recording land in the next one and let a stale Termination
// end the next recording before it had produced anything.
package transcription

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	streamURL = "wss://streaming.assemblyai.com/v3/ws"

	// AssemblyAI bills the wall-clock time a WebSocket stays open - idle time at
	// the same rate as streaming audio - so the settings below are cost
	// decisions, not just behaviour ones.

	// speechModel is pinned because the server-side default is
	// universal-3-5-pro at $0.45/hr, three times the $0.15/hr of this model.
	// Leaving it unset means a default change silently triples the bill.
	speechModel = "universal-streaming-english"

	// audioEncoding matches the PCM16 the recorder produces. It is also the
	// current server default, pinned so a docs update cannot change the wire
	// format underneath us.
	audioEncoding = "pcm_s16le"

	// inactivityTimeout is the server-side backstop, in seconds. If this process
	// dies without terminating, the session dies in 30s rather than staying open
	// to the server's 3-hour cap and being billed for all of it.
	inactivityTimeout = 30

	dialTimeout  = 5 * time.Second
	writeTimeout = 5 * time.Second
)

// ErrSessionClosed reports that the session can no longer accept audio. Callers
// classify with errors.Is rather than matching error strings.
var ErrSessionClosed = errors.New("transcription session closed")

// Result is everything a finished recording produced.
type Result struct {
	Text string

	// FromFinal is false when the text was assembled from partials only,
	// meaning the server never formatted the last turn. The text is usable but
	// unpunctuated, and the caller should not report plain success.
	FromFinal bool

	// Complete is false when the session ended abnormally, so Text may be
	// truncated. Never report success to the user when this is false.
	Complete bool

	// AudioSeconds and SessionSeconds come from the server's Termination
	// message. SessionSeconds is what you are billed for.
	AudioSeconds   float64
	SessionSeconds float64

	// Model is the model the server actually applied, echoed in Begin.
	Model string

	Err error
}

// Session is one recording's connection to AssemblyAI.
type Session struct {
	conn *websocket.Conn

	// gorilla permits one concurrent writer; every write path takes this.
	writeMu sync.Mutex

	mu       sync.Mutex
	finals   map[int]string // formatted transcript per turn
	partials map[int]string // latest unformatted transcript per turn
	model    string
	audioSec float64
	sessSec  float64
	readErr  error

	begun chan struct{}
	term  chan struct{}
	done  chan struct{}

	beginOnce sync.Once
	termOnce  sync.Once
	closeOnce sync.Once
}

// Dial opens a session. It returns as soon as the socket is up; use Begun to
// wait for the server to confirm, so the caller can start capturing audio in
// parallel with the handshake instead of behind it.
func Dial(apiKey string, sampleRate int) (*Session, error) {
	u, err := url.Parse(streamURL)
	if err != nil {
		return nil, fmt.Errorf("parsing stream URL: %w", err)
	}

	q := u.Query()
	q.Set("sample_rate", strconv.Itoa(sampleRate))
	q.Set("encoding", audioEncoding)
	q.Set("speech_model", speechModel)
	q.Set("format_turns", "true")
	q.Set("inactivity_timeout", strconv.Itoa(inactivityTimeout))
	u.RawQuery = q.Encode()

	// DefaultDialer has a 45s handshake timeout and no connect timeout, so a
	// black-holed network (waking before Wi-Fi is up) would hang a press for
	// three quarters of a minute with no feedback.
	dialer := &websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: dialTimeout,
		NetDialContext:   (&net.Dialer{Timeout: dialTimeout}).DialContext,
	}

	conn, resp, err := dialer.Dial(u.String(), http.Header{"Authorization": {apiKey}})
	if err != nil {
		// A 401 and a timeout are different problems; say which.
		if resp != nil {
			return nil, fmt.Errorf("connecting to AssemblyAI (HTTP %d): %w", resp.StatusCode, err)
		}
		return nil, fmt.Errorf("connecting to AssemblyAI: %w", err)
	}

	s := &Session{
		conn:     conn,
		finals:   make(map[int]string),
		partials: make(map[int]string),
		begun:    make(chan struct{}),
		term:     make(chan struct{}),
		done:     make(chan struct{}),
	}

	go s.read()

	return s, nil
}

// Begun is closed once the server has confirmed the session.
func (s *Session) Begun() <-chan struct{} { return s.begun }

// Send streams one chunk of PCM16 audio.
func (s *Session) Send(pcm []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	select {
	case <-s.done:
		return ErrSessionClosed
	default:
	}

	s.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := s.conn.WriteMessage(websocket.BinaryMessage, pcm); err != nil {
		return fmt.Errorf("%w: %v", ErrSessionClosed, err)
	}

	return nil
}

// Finish terminates the session, waits up to wait for the server to flush its
// final transcripts, closes the socket, and returns what the recording
// produced. The session is dead afterwards.
//
// The wait matters: the server finalises the open turn on Terminate, so
// returning early costs formatting on the last turn. The socket is closed
// either way - an un-closed session is billed until the server's 3-hour cap.
func (s *Session) Finish(wait time.Duration) Result {
	termErr := s.sendTerminate()

	if termErr == nil {
		select {
		case <-s.term:
		case <-s.done:
		case <-time.After(wait):
		}
	}

	s.Close()

	// The reader owns finals/partials while it runs. Waiting for it to exit
	// makes the state below final without any further locking dance.
	<-s.done

	text, fromFinal := s.transcript()

	s.mu.Lock()
	res := Result{
		Text:           text,
		FromFinal:      fromFinal,
		AudioSeconds:   s.audioSec,
		SessionSeconds: s.sessSec,
		Model:          s.model,
		Err:            s.readErr,
	}
	s.mu.Unlock()

	if res.Err == nil {
		res.Err = termErr
	}
	res.Complete = res.Err == nil

	return res
}

// Discard terminates and closes the session without waiting for transcripts.
// Used on the skip paths, where the recording is being thrown away but the
// session still has to be closed: an un-terminated one stays open and billed
// until the server's 3-hour cap.
func (s *Session) Discard() {
	s.sendTerminate()
	s.Close()
}

// Close shuts the socket down. Safe to call more than once, and safe to call
// without Finish - Cleanup uses it to guarantee no session outlives the process.
func (s *Session) Close() {
	s.closeOnce.Do(func() {
		s.writeMu.Lock()
		s.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		s.conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		s.writeMu.Unlock()

		s.conn.Close()
	})
}

func (s *Session) sendTerminate() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	payload, err := json.Marshal(map[string]string{"type": "Terminate"})
	if err != nil {
		return fmt.Errorf("encoding terminate message: %w", err)
	}

	s.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := s.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		return fmt.Errorf("sending terminate message: %w", err)
	}

	return nil
}

// transcript composes the recording from the best text available for each turn:
// the formatted final where the server sent one, the latest partial otherwise,
// joined in turn order.
//
// Partials and finals are kept apart on purpose. They used to share one field,
// so a partial for a new turn - or a hallucinated "you" from trailing noise -
// overwrote the entire accumulated transcript, and earlier sentences vanished.
func (s *Session) transcript() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	seen := make(map[int]bool, len(s.finals)+len(s.partials))
	orders := make([]int, 0, len(s.finals)+len(s.partials))
	for turn := range s.finals {
		if !seen[turn] {
			seen[turn] = true
			orders = append(orders, turn)
		}
	}
	for turn := range s.partials {
		if !seen[turn] {
			seen[turn] = true
			orders = append(orders, turn)
		}
	}
	sort.Ints(orders)

	parts := make([]string, 0, len(orders))
	anyFinal := false
	for _, turn := range orders {
		if text, ok := s.finals[turn]; ok {
			parts = append(parts, text)
			anyFinal = true
			continue
		}
		parts = append(parts, s.partials[turn])
	}

	return strings.Join(parts, " "), anyFinal
}

func (s *Session) read() {
	defer close(s.done)

	for {
		_, data, err := s.conn.ReadMessage()
		if err != nil {
			if !isExpectedClose(err) {
				s.mu.Lock()
				s.readErr = err
				s.mu.Unlock()
			}
			return
		}

		s.handle(data)
	}
}

// serverMessage is the subset of the streaming protocol this tool relies on.
// Decoding into a typed struct rather than map[string]any documents those
// fields and surfaces schema drift instead of silently reading zero values.
type serverMessage struct {
	Type       string `json:"type"`
	Transcript string `json:"transcript"`
	TurnOrder  int    `json:"turn_order"`
	Formatted  bool   `json:"turn_is_formatted"`

	Configuration struct {
		SpeechModel string `json:"speech_model"`
	} `json:"configuration"`

	AudioSeconds   float64 `json:"audio_duration_seconds"`
	SessionSeconds float64 `json:"session_duration_seconds"`

	Error string `json:"error"`
}

func (s *Session) handle(data []byte) {
	var msg serverMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return
	}

	switch msg.Type {
	case "Begin":
		s.mu.Lock()
		s.model = msg.Configuration.SpeechModel
		s.mu.Unlock()
		s.beginOnce.Do(func() { close(s.begun) })

	case "Turn":
		if msg.Transcript == "" {
			return
		}
		s.mu.Lock()
		if msg.Formatted {
			s.finals[msg.TurnOrder] = msg.Transcript
		} else {
			s.partials[msg.TurnOrder] = msg.Transcript
		}
		s.mu.Unlock()

	case "Termination":
		s.mu.Lock()
		s.audioSec = msg.AudioSeconds
		s.sessSec = msg.SessionSeconds
		s.mu.Unlock()
		s.termOnce.Do(func() { close(s.term) })

	case "Error":
		s.mu.Lock()
		if s.readErr == nil && msg.Error != "" {
			s.readErr = fmt.Errorf("server error: %s", msg.Error)
		}
		s.mu.Unlock()
	}
}

// isExpectedClose reports whether an error is the ordinary end of a session
// rather than something worth telling the user about. Anything else - including
// a close code like 3007 (chunk duration) or 3008 (session limit) - is kept and
// surfaced, since those used to vanish into a bare "No transcription received".
func isExpectedClose(err error) bool {
	if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
		return true
	}
	return errors.Is(err, net.ErrClosed)
}
