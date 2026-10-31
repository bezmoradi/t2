package transcription

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

var upgrader = &websocket.Upgrader{}

func newTestSession() *Session {
	return &Session{
		finals:   make(map[int]string),
		partials: make(map[int]string),
		begun:    make(chan struct{}),
		term:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

func turn(order int, text string, formatted bool) []byte {
	f := "false"
	if formatted {
		f = "true"
	}
	return []byte(`{"type":"Turn","turn_order":` + strconv.Itoa(order) +
		`,"transcript":"` + text + `","turn_is_formatted":` + f + `}`)
}

// A partial for a later turn must not wipe out an earlier turn's final. This
// lost the first sentence of any dictation with a pause in it whenever the
// server was slow to format the last turn.
func TestPartialAfterFinalKeepsEarlierTurn(t *testing.T) {
	s := newTestSession()
	s.handle(turn(0, "First sentence.", true))
	s.handle(turn(1, "Second sentence", false))

	got, fromFinal := s.transcript()
	if want := "First sentence. Second sentence"; got != want {
		t.Errorf("transcript = %q, want %q", got, want)
	}
	if fromFinal {
		t.Error("fromFinal = true, want false: the last turn is an unformatted partial")
	}
}

// Trailing noise often produces a short partial for a brand new turn. It must
// append, not replace the whole transcript.
func TestTrailingNoisePartialDoesNotReplaceTranscript(t *testing.T) {
	s := newTestSession()
	s.handle(turn(0, "Send the report to the team.", true))
	s.handle(turn(1, "you", false))

	got, _ := s.transcript()
	if !strings.HasPrefix(got, "Send the report to the team.") {
		t.Errorf("transcript = %q, want it to still start with the real sentence", got)
	}
}

// With no finals at all, every turn's latest partial must survive.
func TestPartialsAcrossTurnsAllSurvive(t *testing.T) {
	s := newTestSession()
	s.handle(turn(0, "Hello there my good friend", false))
	s.handle(turn(1, "Yes", false))

	got, fromFinal := s.transcript()
	if want := "Hello there my good friend Yes"; got != want {
		t.Errorf("transcript = %q, want %q", got, want)
	}
	if fromFinal {
		t.Error("fromFinal = true, want false: nothing was formatted")
	}
}

// A final replaces the partial for its own turn.
func TestFinalWinsOverPartialForSameTurn(t *testing.T) {
	s := newTestSession()
	s.handle(turn(0, "hello wold", false))
	s.handle(turn(0, "Hello world.", true))
	s.handle(turn(0, "Hello world.", true)) // resent final

	got, _ := s.transcript()
	if want := "Hello world."; got != want {
		t.Errorf("transcript = %q, want %q", got, want)
	}
}

// Turns compose in numeric order regardless of arrival order.
func TestTurnsComposeInNumericOrder(t *testing.T) {
	s := newTestSession()
	s.handle(turn(2, "Third.", true))
	s.handle(turn(0, "First.", true))
	s.handle(turn(1, "Second.", true))

	got, _ := s.transcript()
	if want := "First. Second. Third."; got != want {
		t.Errorf("transcript = %q, want %q", got, want)
	}
}

// Two sessions share nothing, so a late message from a finished recording can
// never land in the next one.
func TestSessionsAreIsolated(t *testing.T) {
	first := newTestSession()
	second := newTestSession()

	first.handle(turn(0, "Belongs to the first recording.", true))
	second.handle(turn(0, "Belongs to the second.", true))

	got, _ := second.transcript()
	if want := "Belongs to the second."; got != want {
		t.Errorf("second session transcript = %q, want %q", got, want)
	}
}

// A stale Termination cannot satisfy a different session's wait, because the
// signal is per-session rather than a shared channel drained at press time.
func TestTerminationIsPerSession(t *testing.T) {
	first := newTestSession()
	second := newTestSession()

	first.handle([]byte(`{"type":"Termination","audio_duration_seconds":2,"session_duration_seconds":3}`))

	select {
	case <-second.term:
		t.Fatal("a Termination for one session satisfied another session's wait")
	default:
	}

	select {
	case <-first.term:
	default:
		t.Error("the session that received Termination was not signalled")
	}
}

func TestBeginRecordsAppliedModel(t *testing.T) {
	s := newTestSession()
	s.handle([]byte(`{"type":"Begin","id":"x","configuration":{"speech_model":"universal-streaming-english"}}`))

	select {
	case <-s.Begun():
	default:
		t.Fatal("Begun was not closed")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.model != "universal-streaming-english" {
		t.Errorf("model = %q, want the echoed model", s.model)
	}
}

// Finish must terminate and close, so no session is left open and billed.
func TestFinishTerminatesAndClosesTheSocket(t *testing.T) {
	got := make(chan string, 4)
	closed := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer close(closed)
		for {
			mt, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			if mt == websocket.TextMessage {
				got <- string(data)
				// Answer a Terminate the way the real server does.
				c.WriteMessage(websocket.TextMessage,
					[]byte(`{"type":"Termination","audio_duration_seconds":1.5,"session_duration_seconds":2.5}`))
			}
		}
	}))
	defer srv.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	s := newTestSession()
	s.conn = conn
	go s.read()

	s.handle(turn(0, "Hello world.", true))

	res := s.Finish(2 * time.Second)

	select {
	case msg := <-got:
		if !strings.Contains(msg, "Terminate") {
			t.Errorf("server received %q, want a Terminate message", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("server never received a Terminate")
	}

	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Error("socket was not closed; the session would stay open and billed")
	}

	if res.Text != "Hello world." {
		t.Errorf("Text = %q", res.Text)
	}
	if res.SessionSeconds != 2.5 {
		t.Errorf("SessionSeconds = %v, want the server's billed duration", res.SessionSeconds)
	}
	if !res.Complete {
		t.Errorf("Complete = false, err = %v", res.Err)
	}
}

// Discard is the skip path: no transcript wanted, but the session still has to
// be terminated and closed rather than left open.
func TestDiscardClosesTheSocket(t *testing.T) {
	closed := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer close(closed)
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	s := newTestSession()
	s.conn = conn
	go s.read()

	s.Discard()

	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Error("Discard left the socket open; the session would be billed to the 3h cap")
	}
}

// Sending after the session is gone reports a classifiable error rather than a
// string that callers have to pattern-match.
func TestSendAfterCloseIsClassifiable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		c.Close()
	}))
	defer srv.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	s := newTestSession()
	s.conn = conn
	go s.read()

	<-s.done // reader saw the hangup

	if err := s.Send(make([]byte, 64)); err == nil {
		t.Fatal("Send on a dead session returned nil")
	} else if !errors.Is(err, ErrSessionClosed) {
		t.Errorf("Send error %v is not classifiable as ErrSessionClosed", err)
	}
}
