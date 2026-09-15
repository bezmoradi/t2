//go:build darwin

package app

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/bezmoradi/t2/internal/hotkeys"
	"github.com/bezmoradi/t2/internal/transcription"
)

// TestPauseTolerance dictates a paragraph with thinking pauses in the middle
// of its sentences through the speakers (macOS say, with [[slnc N]] embedded
// silences of N milliseconds) and prints the transcript twice: once with the
// server's default turn detection and once with t2's settings, so the
// fragmentation and the fix can be seen side by side. The transcripts come
// from speech recognition of a synthetic voice, so it is a diagnostic to
// read, not an assertion.
//
// Needs a microphone, an API key, and sound on:
//
//	T2_PAUSE_TEST=1 go test ./internal/app -run TestPauseTolerance -v -timeout 5m
func TestPauseTolerance(t *testing.T) {
	if os.Getenv("T2_PAUSE_TEST") == "" {
		t.Skip("set T2_PAUSE_TEST=1 to run (needs a microphone, an API key and sound on)")
	}

	const text = "When I start typing something using this tool [[slnc 1400]] when my voice [[slnc 1200]] " +
		"is broken from my own side [[slnc 1500]] I mean there are a little bit long pauses [[slnc 1300]] " +
		"as you can see my continuous [[slnc 1400]] speech becomes short sentences with the dot at the end"

	dictate := func(label string) string {
		d := NewDaemon(true)
		if err := d.Initialize(); err != nil {
			t.Fatal(err)
		}
		var got string
		d.paste = func(s string) error { got = s; return nil }
		d.copy = d.paste
		d.restart = func() { t.Fatal("restart requested") }

		d.OnPress()
		started := time.Now()
		if err := exec.Command("say", "-r", "185", text).Run(); err != nil {
			t.Fatalf("say: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
		d.OnRelease(hotkeys.Release{Held: time.Since(started)})

		t.Logf("%s\n   -> %s", label, got)
		return got
	}

	t.Setenv("T2_TURN_DEFAULTS", "1")
	before := dictate("with " + transcription.CurrentTurnDetection().String())

	t.Setenv("T2_TURN_DEFAULTS", "")
	after := dictate("with " + transcription.CurrentTurnDetection().String())

	if before == "" || after == "" {
		t.Fatal("no transcript; is the Mac muted, or the microphone permission missing?")
	}
	t.Logf("sentence breaks: %d before, %d after", strings.Count(before, ". "), strings.Count(after, ". "))
}
