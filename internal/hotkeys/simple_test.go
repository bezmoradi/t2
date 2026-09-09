//go:build darwin

package hotkeys

import (
	"sync"
	"testing"
	"time"
)

type spyHandler struct {
	mu        sync.Mutex
	recording bool
	presses   int
	releases  int
	lastHeld  time.Duration
	lastCanc  bool
	slow      time.Duration
}

func (h *spyHandler) OnPress() {
	h.mu.Lock()
	h.recording = true
	h.presses++
	h.mu.Unlock()
	time.Sleep(h.slow) // OnPress really does take a few hundred ms
}

func (h *spyHandler) OnRelease(held time.Duration, cancelled bool) {
	h.mu.Lock()
	h.recording = false
	h.releases++
	h.lastHeld = held
	h.lastCanc = cancelled
	h.mu.Unlock()
}

func (h *spyHandler) snapshot() (bool, int, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.recording, h.presses, h.releases
}

// Edges arriving faster than the handler can process them must still pair up.
// The old two-channel design dropped edges on overflow, which could strand the
// recorder in the on state with no key held.
func TestFastEdgeBurstNeverStrandsRecorder(t *testing.T) {
	h := &spyHandler{slow: 120 * time.Millisecond}
	s := NewSimpleManager(h)

	go s.Listen()
	defer s.Stop()

	const pairs = 6
	now := time.Now()
	for i := 0; i < pairs; i++ {
		s.events <- edge{pressed: true, at: now}
		s.events <- edge{pressed: false, at: now.Add(time.Second)}
	}

	deadline := time.After(5 * time.Second)
	for {
		recording, presses, releases := h.snapshot()
		if presses == pairs && releases == pairs {
			if recording {
				t.Fatal("recorder left running after every release was delivered")
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("edges desynced: presses=%d releases=%d recording=%v", presses, releases, recording)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Hold duration comes from the edge timestamps, not from wall-clock time inside
// the handler. Measuring it in the handler counted the dial, the beep and the
// sleep against the user, so a real one-second hold looked like half a second
// and was discarded as an accidental tap.
func TestHoldDurationComesFromEdgeTimestamps(t *testing.T) {
	h := &spyHandler{slow: 200 * time.Millisecond}
	s := NewSimpleManager(h)

	go s.Listen()
	defer s.Stop()

	press := time.Now()
	s.events <- edge{pressed: true, at: press}
	s.events <- edge{pressed: false, at: press.Add(1500 * time.Millisecond)}

	waitFor(t, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.releases == 1
	})

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lastHeld != 1500*time.Millisecond {
		t.Errorf("held = %v, want exactly 1.5s from the edge timestamps", h.lastHeld)
	}
}

// A hold cancelled by another key going down is reported as such, so the
// daemon can throw the recording away instead of transcribing a shortcut.
func TestCancelledHoldIsReported(t *testing.T) {
	h := &spyHandler{}
	s := NewSimpleManager(h)

	go s.Listen()
	defer s.Stop()

	now := time.Now()
	s.events <- edge{pressed: true, at: now}
	s.events <- edge{pressed: false, at: now.Add(time.Second), cancelled: true}

	waitFor(t, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.releases == 1
	})

	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.lastCanc {
		t.Error("cancelled hold was not reported as cancelled")
	}
}

// A release with no press in flight must not close out the next press.
func TestStrayReleaseIsNotPairedWithNextPress(t *testing.T) {
	h := &spyHandler{}
	s := NewSimpleManager(h)

	go s.Listen()
	defer s.Stop()

	now := time.Now()
	s.events <- edge{pressed: false, at: now}
	time.Sleep(50 * time.Millisecond)
	s.events <- edge{pressed: true, at: now.Add(time.Second)}
	time.Sleep(100 * time.Millisecond)

	recording, presses, releases := h.snapshot()
	if presses != 1 {
		t.Fatalf("presses = %d, want 1", presses)
	}
	if releases != 0 {
		t.Errorf("releases = %d, want 0: a stray release closed out a press it never belonged to", releases)
	}
	if !recording {
		t.Error("recorder stopped while the key is still held")
	}
}

func TestStopUnblocksListen(t *testing.T) {
	s := NewSimpleManager(&spyHandler{})
	done := make(chan struct{})
	go func() { s.Listen(); close(done) }()

	s.Stop()
	s.Stop() // must be safe twice (Cleanup path)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Listen did not return after Stop")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		if cond() {
			return
		}
		select {
		case <-deadline:
			t.Fatal("condition not met in time")
		case <-time.After(5 * time.Millisecond):
		}
	}
}
