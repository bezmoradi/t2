package app

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
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

	// flushTimeout bounds the wait, after the key is released, for audio the
	// sender has not yet delivered. It is normally zero: audio streams live.
	// It is only ever long when the dial was slow and the backlog is large, or
	// the network has stalled, in which case the server transcribes what it got.
	flushTimeout = 3 * time.Second

	// restartLimit within restartWindow is where automatic restarts stop, so a
	// broken audio setup cannot make t2 restart after every recording forever.
	restartLimit  = 3
	restartWindow = time.Hour
)

// restartGrace is how long a wedged process waits before restarting itself,
// in case the stream shutdown was merely slow and completes after all. A
// variable so tests can shorten it.
var restartGrace = 10 * time.Second

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

	// paste delivers a transcript to the active application, and copy only
	// puts it on the clipboard. Fields so the soak test can run the whole
	// pipeline without typing into whatever window happens to be focused.
	paste func(string) error
	copy  func(string) error

	// restart replaces the process after the audio layer has wedged. A field
	// for the same reason: the soak test must not re-exec itself.
	restart func()

	// beep and dial are fields so tests can run the daemon silently and
	// without a network.
	beep func(string)
	dial func(apiKey string, sampleRate int) (*transcription.Session, error)

	dialDone  chan dialResult
	listenEnd chan struct{}
}

func NewDaemon(verbose bool) *Daemon {
	return &Daemon{
		sink:      newAudioSink(),
		verbose:   verbose,
		paste:     clipboard.PasteTextSafely,
		copy:      clipboard.CopyText,
		restart:   restartProcess,
		beep:      audio.PlayBeep,
		dial:      transcription.Dial,
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
	// do not abandon a session that is still being billed.
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
}

// OnPress implements hotkeys.EventHandler
func (d *Daemon) OnPress() {
	if d.recorder.IsRecording() {
		return
	}

	// A press while the audio layer is wedged: nothing can be recorded, so
	// there is no point waiting out the restart grace period, and no beep
	// either - a beep player still alive at exec would be left as a zombie.
	if audio.Wedged() {
		d.restart()
		return
	}

	d.beep("start")

	// Capture first, dial second. The handshake costs 60-140ms of TLS plus an
	// HTTP upgrade; if capture waited for it, every recording would lose its
	// opening syllable. Audio buffers in the sink until the session is ready.
	d.sink.start()
	if err := d.recorder.Start(); err != nil {
		d.sink.stop()
		fmt.Printf("❌ Could not start recording: %v\n", err)
		if errors.Is(err, audio.ErrWedged) {
			d.restart()
			return
		}
		fmt.Println("   Check that this terminal has Microphone permission.")
		fmt.Println()
		return
	}

	d.dialDone = make(chan dialResult, 1)
	go func(done chan dialResult) {
		started := time.Now()

		session, err := d.dial(d.apiKey, audio.SampleRate)
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
func (d *Daemon) OnRelease(rel hotkeys.Release) {
	if !d.recorder.IsRecording() {
		return
	}

	// The beep answers the key release, so it goes before the stop: a wedged
	// stop takes seconds to give up, and that used to sound like no beep.
	d.beep("stop")
	outcome := d.recorder.Stop()

	// Whatever happens below, the sender must not outlive this recording.
	defer d.sink.stop()

	if outcome.Wedged {
		// The audio is intact; only the shutdown hung. Finish this recording
		// first - transcript pasted, session closed - then deal with it.
		defer d.scheduleRestart()
	}

	if rel.Capped {
		// The poller ended the hold with the keys still down. Said before the
		// result, on every path, so that a modifier stuck in the down state -
		// which produces exactly this once and then silence - is explained
		// rather than mysterious.
		fmt.Printf("⏱  Hold reached the %v limit - recording stopped. Release the keys, then press again to continue.\n", hotkeys.MaxHold)
	}

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

	if rel.Cancelled {
		session.Discard()
		fmt.Println("⌨️  Shortcut detected - skipped")
		fmt.Println()
		return
	}

	if rel.Held < quickPressThreshold {
		session.Discard()
		fmt.Printf("⚡ Quick press (%v) - skipped\n", rel.Held.Round(time.Millisecond))
		fmt.Println()
		return
	}

	if outcome.Chunks == 0 {
		// A silent room still produces chunks. None at all means the device
		// ran no IO: the wrong input selected, or one that is gone.
		session.Discard()
		fmt.Println("❌ The microphone delivered no audio - check the input device under System Settings > Sound")
		fmt.Println()
		return
	}

	if outcome.MaxRMS < audio.SilenceThreshold {
		session.Discard()
		fmt.Printf("🔇 No speech detected - skipped (level %.0f, threshold %.0f)\n",
			outcome.MaxRMS, audio.SilenceThreshold)
		fmt.Println()
		return
	}

	// Everything captured has to reach the server before Terminate, or the
	// end of the recording is never transcribed. Then the sender is stopped so
	// nothing is written to the socket after Terminate.
	d.sink.flush(flushTimeout)
	d.sink.stop()

	transcript := session.Finish(terminationTimeout)

	d.logf("held %v, %d chunks, %d overflow(s), model %q, audio %.1fs, billed %.1fs",
		rel.Held.Round(time.Millisecond), outcome.Chunks, outcome.Overflows,
		transcript.Model, transcript.AudioSeconds, transcript.SessionSeconds)

	d.report(transcript, outcome, rel.Capped)
}

// report pastes the transcript and says plainly what was produced.
//
// The old code printed a green check for any non-empty text, so a truncated
// capture, a dropped connection and an unformatted partial all looked like
// success. Anything short of a clean formatted result now says so.
func (d *Daemon) report(t transcription.Result, outcome audio.Outcome, capped bool) {
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

	if capped {
		// The keys are still physically down: a synthesised Cmd+V would arrive
		// as Ctrl+Shift+Cmd+V and do who knows what in the focused app. Copy
		// only, and let the user paste once they let go.
		if err := d.copy(t.Text); err != nil {
			fmt.Printf("❌ Copy failed: %v\n", err)
		} else {
			fmt.Println("📋 Copied to the clipboard - release the keys, then press Cmd+V to paste")
		}
		fmt.Printf("   %s\n", t.Text)
		return
	}

	if err := d.paste(t.Text); err != nil {
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

// scheduleRestart deals with a wedged audio layer once the recording that hit
// it has been reported. It waits out a grace period first: a stop that was
// merely slow completes in that time and nothing needs to happen, while a
// press during the wait restarts at once (see OnPress).
func (d *Daemon) scheduleRestart() {
	grace := restartGrace
	fmt.Printf("⚠️  The microphone did not shut down (CoreAudio hang). Restarting t2 in %v unless it recovers...\n", grace)
	fmt.Println()

	go func() {
		deadline := time.Now().Add(grace)
		for time.Now().Before(deadline) {
			if !audio.Wedged() {
				fmt.Println("✅ The microphone shut down after all - no restart needed")
				fmt.Println()
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		d.restart()
	}()
}

// Environment markers that survive the exec, so a restart loop can be noticed.
const (
	restartCountEnv = "T2_RESTART_COUNT"
	restartLastEnv  = "T2_RESTART_LAST"
)

// restartProcess replaces the process with a fresh copy of itself.
//
// This is the recovery for the CoreAudio deadlock described at
// audio.Recorder.Stop. Once a stream is stuck in it, nothing on that device
// can be started again in this process, so a restart is the only way back to
// a working microphone. Doing it here, automatically, turns what used to be
// "t2 silently stops responding until you notice and restart it" into a few
// seconds' pause and a line of output. The recording that hit the deadlock
// has already been transcribed and pasted by the time this runs.
//
// It gives up after restartLimit restarts within restartWindow: past that the
// deadlock is not the occasional race it is meant for but something broken,
// and restarting after every recording would only hide it.
func restartProcess() {
	count, _ := strconv.Atoi(os.Getenv(restartCountEnv))
	last, _ := strconv.ParseInt(os.Getenv(restartLastEnv), 10, 64)
	if time.Since(time.Unix(last, 0)) > restartWindow {
		count = 0
	}
	count++

	if count > restartLimit {
		fmt.Printf("❌ The audio layer deadlocked again - %d restarts in the last hour, so not restarting automatically any more.\n", count-1)
		fmt.Println("   Please restart t2 by hand, and report this if it keeps happening.")
		fmt.Println()
		return
	}

	fmt.Println("🔁 The audio layer hung inside CoreAudio. Restarting t2 automatically...")
	fmt.Println()

	os.Setenv(restartCountEnv, strconv.Itoa(count))
	os.Setenv(restartLastEnv, strconv.FormatInt(time.Now().Unix(), 10))

	exe, err := os.Executable()
	if err == nil {
		// Exec only returns on failure. Every thread, the stuck one included,
		// dies with the old image; the new process starts clean.
		err = syscall.Exec(exe, os.Args, os.Environ())
	}

	fmt.Printf("❌ Automatic restart failed: %v\n", err)
	fmt.Println("   Please restart t2 by hand.")
	fmt.Println()
}

func (d *Daemon) logf(format string, args ...any) {
	if !d.verbose {
		return
	}
	fmt.Printf("   [v] "+format+"\n", args...)
}
