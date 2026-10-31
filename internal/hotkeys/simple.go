//go:build (darwin && cgo) || linux

package hotkeys

import (
	"sync"
	"time"
)

const (
	pollInterval      = 100 * time.Millisecond
	edgeQueueCapacity = 64
)

type edge struct {
	pressed             bool
	at                  time.Time
	cancelledByOtherKey bool
	cappedAtMaxHold     bool
}

type Manager struct {
	handler  EventHandler
	edges    chan edge
	done     chan struct{}
	stopOnce sync.Once
}

func NewManager(handler EventHandler) *Manager {
	return &Manager{
		handler: handler,
		edges:   make(chan edge, edgeQueueCapacity),
		done:    make(chan struct{}),
	}
}

func (s *Manager) Start() error {
	if err := openKeyboard(); err != nil {
		return err
	}
	go s.pollKeyState()
	return nil
}

func (s *Manager) Source() string { return keyboardSource() }

func (s *Manager) Stop() {
	s.stopOnce.Do(func() { close(s.done) })
}

func (s *Manager) Listen() {
	for {
		select {
		case press := <-s.edges:
			releaseWithoutPress := !press.pressed
			if releaseWithoutPress {
				continue
			}

			if s.handler != nil {
				s.handler.OnPress()
			}

			release, ok := s.waitForRelease()
			if !ok {
				return
			}

			if s.handler != nil {
				s.handler.OnRelease(Release{
					HeldFor:             release.at.Sub(press.at),
					CancelledByOtherKey: release.cancelledByOtherKey,
					CappedAtMaxHold:     release.cappedAtMaxHold,
				})
			}
		case <-s.done:
			return
		}
	}
}

func (s *Manager) waitForRelease() (edge, bool) {
	for {
		select {
		case next := <-s.edges:
			if !next.pressed {
				return next, true
			}
		case <-s.done:
			return edge{}, false
		}
	}
}

func (s *Manager) pollKeyState() {
	var (
		wasPressed     bool
		pressedAt      time.Time
		endedWhileHeld bool
	)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
		}

		now := time.Now()
		isPressed := ctrlShiftDown()

		switch {
		case isPressed && !wasPressed:
			startedWithAnotherKeyDown := otherKeyDown()
			if startedWithAnotherKeyDown {
				continue
			}
			wasPressed = true
			pressedAt = now
			if !s.emitBlocking(edge{pressed: true, at: now}) {
				return
			}

		case isPressed && wasPressed:
			if endedWhileHeld {
				continue
			}
			shortcutPressed := otherKeyDown()
			if shortcutPressed {
				endedWhileHeld = true
				if !s.emitBlocking(edge{pressed: false, at: now, cancelledByOtherKey: true}) {
					return
				}
				continue
			}
			reachedMaxHold := now.Sub(pressedAt) >= MaxHold
			if reachedMaxHold {
				endedWhileHeld = true
				if !s.emitBlocking(edge{pressed: false, at: now, cappedAtMaxHold: true}) {
					return
				}
			}

		case !isPressed && wasPressed:
			wasPressed = false
			if endedWhileHeld {
				endedWhileHeld = false
				continue
			}
			if !s.emitBlocking(edge{pressed: false, at: now}) {
				return
			}
		}
	}
}

func (s *Manager) emitBlocking(e edge) bool {
	select {
	case s.edges <- e:
		return true
	case <-s.done:
		return false
	}
}
