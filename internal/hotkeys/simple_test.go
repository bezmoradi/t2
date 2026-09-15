//go:build darwin

package hotkeys

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type spyHandler struct {
	mu        sync.Mutex
	recording bool
	presses   int
	releases  int
	lastRel   Release
	slow      time.Duration
}

func (h *spyHandler) OnPress() {
	h.mu.Lock()
	h.recording = true
	h.presses++
	h.mu.Unlock()
	time.Sleep(h.slow) // OnPress really does take a few hundred ms
}

func (h *spyHandler) OnRelease(rel Release) {
	h.mu.Lock()
	h.recording = false
	h.releases++
	h.lastRel = rel
	h.mu.Unlock()
}

func (h *spyHandler) snapshot() (bool, int, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.recording, h.presses, h.releases
}

func (h *spyHandler) last() Release {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastRel
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
		_, _, releases := h.snapshot()
		return releases == 1
	})

	if held := h.last().Held; held != 1500*time.Millisecond {
		t.Errorf("held = %v, want exactly 1.5s from the edge timestamps", held)
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
		_, _, releases := h.snapshot()
		return releases == 1
	})

	if !h.last().Cancelled {
		t.Error("cancelled hold was not reported as cancelled")
	}
}

// A release produced at the hold limit is reported as capped, so the daemon
// can tell the user why the recording ended with the keys still down.
func TestCappedReleaseIsReported(t *testing.T) {
	h := &spyHandler{}
	s := NewSimpleManager(h)

	go s.Listen()
	defer s.Stop()

	now := time.Now()
	s.events <- edge{pressed: true, at: now}
	s.events <- edge{pressed: false, at: now.Add(MaxHold), capped: true}

	waitFor(t, func() bool {
		_, _, releases := h.snapshot()
		return releases == 1
	})

	if rel := h.last(); !rel.Capped || rel.Held != MaxHold {
		t.Errorf("got %+v, want a capped release held for %v", rel, MaxHold)
	}
}

// A hold that reaches the limit is released by the poller itself, and the keys
// are then ignored until they physically come up: no second recording may
// start off a modifier that is stuck down, and the eventual real release must
// not be reported a second time. Only a fresh press starts a new hold.
func TestHoldIsCappedAndKeysIgnoredUntilReleased(t *testing.T) {
	var down atomic.Bool
	h := &spyHandler{}
	s := NewSimpleManager(h)
	s.hotkeyDown = down.Load
	s.otherDown = func() bool { return false }
	s.maxHold = 300 * time.Millisecond

	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	go s.Listen()
	defer s.Stop()

	down.Store(true)
	waitFor(t, func() bool {
		_, _, releases := h.snapshot()
		return releases == 1
	})

	rel := h.last()
	if !rel.Capped {
		t.Fatal("release at the limit was not reported as capped")
	}
	if rel.Held < s.maxHold {
		t.Errorf("held = %v, want at least the %v limit", rel.Held, s.maxHold)
	}

	// Keys still down: nothing further may happen.
	time.Sleep(4 * pollInterval)
	if _, presses, releases := h.snapshot(); presses != 1 || releases != 1 {
		t.Fatalf("keys still down after the cap: presses=%d releases=%d, want 1 and 1", presses, releases)
	}

	// The physical release was already reported at the limit.
	down.Store(false)
	time.Sleep(4 * pollInterval)
	if _, presses, releases := h.snapshot(); presses != 1 || releases != 1 {
		t.Fatalf("after the physical release: presses=%d releases=%d, want 1 and 1", presses, releases)
	}

	// A fresh press is a new hold.
	down.Store(true)
	waitFor(t, func() bool {
		_, presses, _ := h.snapshot()
		return presses == 2
	})
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
