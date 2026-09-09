package app

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/bezmoradi/t2/internal/audio"
	"github.com/bezmoradi/t2/internal/clipboard"
	"github.com/bezmoradi/t2/internal/config"
	"github.com/bezmoradi/t2/internal/hotkeys"
	"github.com/bezmoradi/t2/internal/transcription"
)

const (
	// quickPressThreshold discards taps too short to be dictation. It is
	// measured from the poller's edge timestamps, so it reflects the real hold
	// rather than whatever time was left after dialling and beeping. It can be
	// this short because shortcuts like Ctrl+Shift+T are now rejected outright
	// by the hotkey layer instead of being absorbed here.
	quickPressThreshold = 400 * time.Millisecond

	// terminationTimeout is a safety net for an unresponsive server, not a
	// latency knob. The server finalises the open turn when it receives
	// Terminate, so cutting this short costs punctuation on the last turn.
	terminationTimeout = 4 * time.Second

	// beginTimeout bounds the wait for the server to confirm the session
	// before streaming buffered audio at it.
	beginTimeout = 5 * time.Second
)

type dialResult struct {
	session *transcription.Session
	err     error
}

type Daemon struct {
	recorder      *audio.Recorder
	hotkeyManager *hotkeys.Manager
	sink          *audioSink
	apiKey        string
	verbose       bool

	dialDone  chan dialResult
	listenEnd chan struct{}
}

func NewDaemon(verbose bool) *Daemon {
	return &Daemon{
		sink:      newAudioSink(),
		verbose:   verbose,
		listenEnd: make(chan struct{}),
	}
}

func (d *Daemon) Initialize() error {
	var err error
	d.apiKey, err = config.GetAPIKey()
	if err != nil {
		return fmt.Errorf("failed to get AssemblyAI API key: %v", err)
	}

	d.recorder = audio.NewRecorder(d.sink.write)
	d.hotkeyManager = hotkeys.NewManager(d)

	if err := audio.Initialize(); err != nil {
		return fmt.Errorf("failed to initialize PortAudio: %v", err)
	}

	// No session is opened here. AssemblyAI bills the wall-clock time a
	// connection stays open, so a session dialled at startup and left idle
	// until the first recording is billed for the entire wait.

	return nil
}

func (d *Daemon) Run() error {
	if err := d.hotkeyManager.Start(); err != nil {
		return fmt.Errorf("failed to start hotkey: %v", err)
	}

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	fmt.Println("🎤 T2 - Voice-to-Text Daemon Started")
	fmt.Printf("📋 Hold %s to record, release to transcribe & paste\n", d.hotkeyManager.GetHotkeyDisplay())
	fmt.Println("🛑 Press Ctrl+C to exit")
	fmt.Println()

	go func() {
		defer close(d.listenEnd)
		d.hotkeyManager.Listen()
	}()

	<-c
	fmt.Println("\n🛑 Shutting down...")
	d.Cleanup()

	return nil
}

func (d *Daemon) Cleanup() {
	// Stop feeding the handler, then let any in-flight recording finish, so we
	// do not tear PortAudio down underneath a running capture or abandon a
	// session that is still being billed.
	if d.hotkeyManager != nil {
		d.hotkeyManager.Stop()
		select {
		case <-d.listenEnd:
		case <-time.After(terminationTimeout + 2*time.Second):
		}
	}

	if d.recorder != nil {
		d.recorder.Stop()
	}

	audio.Terminate()
}

// OnPress implements hotkeys.EventHandler
func (d *Daemon) OnPress() {
	if d.recorder.IsRecording() {
		return
	}

	audio.PlayBeep("start")

	// Capture first, dial second. The handshake costs 60-140ms of TLS plus an
	// HTTP upgrade; if capture waited for it, every recording would lose its
	// opening syllable. Audio buffers in the sink until the session is ready.
	d.sink.reset()
	if err := d.recorder.Start(); err != nil {
		fmt.Printf("❌ Could not start recording: %v\n", err)
		fmt.Println("   Check that this terminal has Microphone permission.")
		fmt.Println()
		return
	}

	d.dialDone = make(chan dialResult, 1)
	go func(done chan dialResult) {
		started := time.Now()

		session, err := transcription.Dial(d.apiKey, audio.SampleRate)
		if err != nil {
			done <- dialResult{err: err}
			return
		}

		// Wait for Begin rather than sleeping a fixed 150ms: it is the actual
		// signal that the session exists and can accept audio.
		select {
		case <-session.Begun():
		case <-time.After(beginTimeout):
		}

		d.sink.attach(session)
		d.logf("dial+begin took %v", time.Since(started).Round(time.Millisecond))

		done <- dialResult{session: session}
	}(d.dialDone)
}

// OnRelease implements hotkeys.EventHandler
func (d *Daemon) OnRelease(held time.Duration, cancelled bool) {
	if !d.recorder.IsRecording() {
		return
	}

	outcome := d.recorder.Stop()
	audio.PlayBeep("stop")

	// The dial may still be in flight; it owns the session either way.
	result := <-d.dialDone
	d.dialDone = nil

	if result.err != nil {
		fmt.Printf("❌ Could not reach AssemblyAI: %v\n", result.err)
		d.saveAudio()
		fmt.Println()
		return
	}

	session := result.session

	// Every path from here closes the session. One left open stays billed
	// until the server's 3-hour cap.

	if cancelled {
		session.Discard()
		fmt.Println("⌨️  Shortcut detected - skipped")
		fmt.Println()
		return
	}

	if held < quickPressThreshold {
		session.Discard()
		fmt.Printf("⚡ Quick press (%v) - skipped\n", held.Round(time.Millisecond))
		fmt.Println()
		return
	}

	if maxRMS := d.recorder.GetMaxRMS(); maxRMS < audio.SilenceThreshold {
		session.Discard()
		fmt.Printf("🔇 No speech detected - skipped (level %.0f, threshold %.0f)\n",
			maxRMS, audio.SilenceThreshold)
		fmt.Println()
		return
	}

	transcript := session.Finish(terminationTimeout)

	d.logf("held %v, %d chunks, %d overflow(s), model %q, audio %.1fs, billed %.1fs",
		held.Round(time.Millisecond), outcome.Chunks, outcome.Overflows,
		transcript.Model, transcript.AudioSeconds, transcript.SessionSeconds)

	d.report(transcript, outcome)
}

// report pastes the transcript and says plainly what was produced.
//
// The old code printed a green check for any non-empty text, so a truncated
// capture, a dropped connection and an unformatted partial all looked like
// success. Anything short of a clean formatted result now says so.
func (d *Daemon) report(t transcription.Result, outcome audio.Outcome) {
	defer fmt.Println()

	sendErr := d.sink.err()

	if t.Text == "" {
		fmt.Println("❌ No transcription received")
		if t.Err != nil {
			fmt.Printf("   %v\n", t.Err)
		}
		if sendErr != nil {
			fmt.Printf("   audio streaming stopped: %v\n", sendErr)
		}
		d.saveAudio()
		return
	}

	if err := clipboard.PasteTextSafely(t.Text); err != nil {
		fmt.Printf("❌ Paste failed: %v\n", err)
		fmt.Printf("   Transcript: %s\n", t.Text)
		return
	}

	switch {
	case sendErr != nil:
		fmt.Println("⚠️  Pasted, but the connection dropped mid-recording - text may be incomplete")
		fmt.Printf("   %v\n", sendErr)
		d.saveAudio()
	case outcome.Err != nil:
		fmt.Println("⚠️  Pasted, but recording stopped early - text may be incomplete")
		fmt.Printf("   %v\n", outcome.Err)
		d.saveAudio()
	case !t.Complete:
		// Carries the server's close code and reason, which used to disappear
		// into a bare "No transcription received".
		fmt.Println("⚠️  Pasted, but the session ended abnormally - text may be incomplete")
		fmt.Printf("   %v\n", t.Err)
		d.saveAudio()
	case outcome.Overflows > 0:
		fmt.Printf("⚠️  Pasted, but %d audio chunk(s) were dropped - text may have gaps\n", outcome.Overflows)
	case !t.FromFinal:
		fmt.Println("⚠️  Pasted an unformatted draft - the server did not finish in time")
	default:
		fmt.Println("✅ Pasted to active application")
	}

	// Print the text too, so it survives a clipboard that gets overwritten
	// before the user notices the paste landed somewhere unexpected.
	fmt.Printf("   %s\n", t.Text)
}

// saveAudio writes the captured recording to disk, so a failure anywhere
// downstream never costs the user what they already said.
func (d *Daemon) saveAudio() {
	pcm := d.sink.audio()
	if len(pcm) == 0 {
		return
	}

	dir, err := config.GetConfigDir()
	if err != nil {
		return
	}

	path := filepath.Join(dir, "last-recording.wav")
	if err := audio.WriteWAV(path, pcm); err != nil {
		fmt.Printf("   (could not save audio: %v)\n", err)
		return
	}

	fmt.Printf("   Audio saved to %s\n", path)
}

func (d *Daemon) logf(format string, args ...any) {
	if !d.verbose {
		return
	}
	fmt.Printf("   [v] "+format+"\n", args...)
}
