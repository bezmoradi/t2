package transcription

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

const (
	streamURL = "wss://streaming.assemblyai.com/v3/ws"

	speechModel   = "universal-streaming-english"
	audioEncoding = "pcm_s16le"

	serverInactivityTimeoutSeconds = 30

	dialTimeout  = 5 * time.Second
	writeTimeout = 5 * time.Second

	maxServerMessageBytes = 1 << 20
	maxTextBytes          = 64 << 10

	maxTurnSilenceMS    = 5000
	minTurnSilenceMS    = 700
	endOfTurnConfidence = 0.85
)

const (
	maxTurnSilenceEnv      = "T2_MAX_TURN_SILENCE_MS"
	minTurnSilenceEnv      = "T2_MIN_TURN_SILENCE_MS"
	endOfTurnConfidenceEnv = "T2_END_OF_TURN_CONFIDENCE"
	turnDefaultsEnv        = "T2_TURN_DEFAULTS"
)

type TurnDetection struct {
	MaxTurnSilenceMS    int
	MinTurnSilenceMS    int
	EndOfTurnConfidence float64
	ServerDefaults      bool
}

func CurrentTurnDetection() TurnDetection {
	td := TurnDetection{
		MaxTurnSilenceMS:    maxTurnSilenceMS,
		MinTurnSilenceMS:    minTurnSilenceMS,
		EndOfTurnConfidence: endOfTurnConfidence,
	}
	if os.Getenv(turnDefaultsEnv) != "" {
		td.ServerDefaults = true
		return td
	}
	if v, err := strconv.Atoi(os.Getenv(maxTurnSilenceEnv)); err == nil && v > 0 {
		td.MaxTurnSilenceMS = v
	}
	if v, err := strconv.Atoi(os.Getenv(minTurnSilenceEnv)); err == nil && v > 0 {
		td.MinTurnSilenceMS = v
	}
	if v, err := strconv.ParseFloat(os.Getenv(endOfTurnConfidenceEnv), 64); err == nil && v > 0 && v <= 1 {
		td.EndOfTurnConfidence = v
	}
	return td
}

func (td TurnDetection) apply(q url.Values) {
	if td.ServerDefaults {
		return
	}
	q.Set("max_turn_silence", strconv.Itoa(td.MaxTurnSilenceMS))
	q.Set("min_turn_silence", strconv.Itoa(td.MinTurnSilenceMS))
	q.Set("end_of_turn_confidence_threshold", strconv.FormatFloat(td.EndOfTurnConfidence, 'f', 2, 64))
}

var ErrSessionClosed = errors.New("transcription session closed")

type Result struct {
	Text              string
	LastTurnFormatted bool
	EndedCleanly      bool
	AudioSeconds      float64
	BilledSeconds     float64
	Model             string
	Err               error
}

type Session struct {
	conn *websocket.Conn

	writeMu sync.Mutex

	mu                   sync.Mutex
	formattedByTurn      map[int]string
	latestDraftByTurn    map[int]string
	model                string
	audioSeconds         float64
	billedSessionSeconds float64
	firstErr             error

	begun        chan struct{}
	terminated   chan struct{}
	readerExited chan struct{}

	beginOnce      sync.Once
	terminatedOnce sync.Once
	closeOnce      sync.Once
}

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
	q.Set("inactivity_timeout", strconv.Itoa(serverInactivityTimeoutSeconds))
	CurrentTurnDetection().apply(q)
	u.RawQuery = q.Encode()

	dialer := &websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: dialTimeout,
		NetDialContext:   (&net.Dialer{Timeout: dialTimeout}).DialContext,
	}

	conn, resp, err := dialer.Dial(u.String(), http.Header{"Authorization": {apiKey}})
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("connecting to AssemblyAI (HTTP %d): %w", resp.StatusCode, err)
		}
		return nil, fmt.Errorf("connecting to AssemblyAI: %w", err)
	}
	conn.SetReadLimit(maxServerMessageBytes)

	s := &Session{
		conn:              conn,
		formattedByTurn:   make(map[int]string),
		latestDraftByTurn: make(map[int]string),
		begun:             make(chan struct{}),
		terminated:        make(chan struct{}),
		readerExited:      make(chan struct{}),
	}

	go s.read()

	return s, nil
}

func (s *Session) Begun() <-chan struct{} { return s.begun }

func (s *Session) Done() <-chan struct{} { return s.readerExited }

func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.firstErr
}

func (s *Session) Send(pcm []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if isClosed(s.readerExited) {
		return ErrSessionClosed
	}

	s.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := s.conn.WriteMessage(websocket.BinaryMessage, pcm); err != nil {
		return fmt.Errorf("%w: %v", ErrSessionClosed, err)
	}

	return nil
}

func (s *Session) Finish(wait time.Duration) Result {
	termErr := s.sendTerminate()

	if termErr == nil {
		select {
		case <-s.terminated:
		case <-s.readerExited:
		case <-time.After(wait):
		}

		if !isClosed(s.terminated) {
			termErr = fmt.Errorf("the server did not confirm the end of the session within %v", wait)
		}
	}

	s.Close()

	<-s.readerExited

	text, lastTurnFormatted := s.transcript()

	s.mu.Lock()
	res := Result{
		Text:              text,
		LastTurnFormatted: lastTurnFormatted,
		AudioSeconds:      s.audioSeconds,
		BilledSeconds:     s.billedSessionSeconds,
		Model:             s.model,
		Err:               s.firstErr,
	}
	s.mu.Unlock()

	if res.Err == nil {
		res.Err = termErr
	}
	res.EndedCleanly = res.Err == nil

	return res
}

func (s *Session) Discard() {
	s.sendTerminate()
	s.Close()
}

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

func (s *Session) transcript() (text string, lastTurnFormatted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	seen := make(map[int]bool, len(s.formattedByTurn)+len(s.latestDraftByTurn))
	turns := make([]int, 0, len(s.formattedByTurn)+len(s.latestDraftByTurn))
	for turn := range s.formattedByTurn {
		if !seen[turn] {
			seen[turn] = true
			turns = append(turns, turn)
		}
	}
	for turn := range s.latestDraftByTurn {
		if !seen[turn] {
			seen[turn] = true
			turns = append(turns, turn)
		}
	}
	sort.Ints(turns)

	parts := make([]string, 0, len(turns))
	for _, turn := range turns {
		best, formatted := s.formattedByTurn[turn]
		if !formatted {
			best = s.latestDraftByTurn[turn]
		}
		parts = append(parts, best)
		lastTurnFormatted = formatted
	}

	return strings.Join(parts, " "), lastTurnFormatted
}

func (s *Session) read() {
	defer close(s.readerExited)

	for {
		_, data, err := s.conn.ReadMessage()
		if err != nil {
			if !isExpectedClose(err) {
				s.keepFirstError(err)
			}
			return
		}

		s.handle(data)
	}
}

func (s *Session) keepFirstError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.firstErr == nil {
		s.firstErr = err
	}
}

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
		text := Sanitize(msg.Transcript)
		if text == "" {
			return
		}
		s.mu.Lock()
		if msg.Formatted {
			s.formattedByTurn[msg.TurnOrder] = text
		} else {
			s.latestDraftByTurn[msg.TurnOrder] = text
		}
		s.mu.Unlock()

	case "Termination":
		s.mu.Lock()
		s.audioSeconds = msg.AudioSeconds
		s.billedSessionSeconds = msg.SessionSeconds
		s.mu.Unlock()
		s.terminatedOnce.Do(func() { close(s.terminated) })

	case "Error":
		if msg.Error != "" {
			s.keepFirstError(fmt.Errorf("server error: %s", Sanitize(msg.Error)))
		}
	}
}

func Sanitize(text string) string {
	var b strings.Builder
	b.Grow(min(len(text), maxTextBytes))
	for _, r := range text {
		switch {
		case isTabOrLineBreak(r):
			r = ' '
		case isControlCharacter(r), isBidiOverrideOrIsolate(r):
			continue
		}
		wouldExceedCap := b.Len()+utf8.RuneLen(r) > maxTextBytes
		if wouldExceedCap {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isTabOrLineBreak(r rune) bool { return r == '\t' || r == '\n' || r == '\r' }

func isControlCharacter(r rune) bool { return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) }

func isBidiOverrideOrIsolate(r rune) bool {
	return (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069)
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func isExpectedClose(err error) bool {
	if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
		return true
	}
	return errors.Is(err, net.ErrClosed)
}
