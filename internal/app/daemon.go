package app

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	// rather than whatever time was left after beeping. It is also when the
	// connection to AssemblyAI opens: capture starts on the press, but a hold
	// shorter than this never opens a billed session or sends any audio.
	quickPressThreshold = 400 * time.Millisecond

	// keepCancelledAfter is the hold length past which a hold ended by
	// another key counts as interrupted dictation rather than a shortcut, so
	// its audio is saved instead of thrown away.
	keepCancelledAfter = 3 * time.Second

	// terminationTimeout is a safety net for an unresponsive server, not a
	// latency knob. The server finalises the open turn when it receives
	// Terminate, so cutting this short costs punctuation on the last turn.
	terminationTimeout = 4 * time.Second

	// beginTimeout bounds the wait for the server to confirm the session
	// before streaming buffered audio at it.
	beginTimeout = 5 * time.Second

	// shutdownWait bounds how long Ctrl+C waits for recordings that are
	// still being finished. A second Ctrl+C exits at once.
	shutdownWait = 15 * time.Second

	// restartLimit within restartWindow is where automatic restarts stop, so a
	// broken audio setup cannot make t2 restart after every recording forever.
	restartLimit  = 3
	restartWindow = time.Hour

	// flushTimeout bounds the wait, after the key is released, for audio the
	// sender has not yet delivered. It is normally zero: audio streams live.
	// It is only ever long when the dial was slow and the backlog is large, or
	// the network has stalled; what did not make it is reported and saved.
	flushTimeout = 3 * time.Second

	// keysUpTimeout bounds how long a finished transcript waits for the
	// hotkey to be let go before it is pasted.
	keysUpTimeout = hotkeys.MaxHold

	// savedAudioName is the fallback recording in the config directory. There
	// is one, overwritten by each failure and deleted by the next success.
	savedAudioName = "last-recording.wav"
)

// restartGrace is how long a wedged process waits before restarting itself,
// in case the stream shutdown was merely slow and completes after all. A
// variable so tests can shorten it.
var restartGrace = 10 * time.Second

type dialResult struct {
	session *transcription.Session // set whenever a socket was opened, so it can be closed
	err     error
}

// take is one hold: its audio and its connection to AssemblyAI. There is one
// per hold rather than one at a time, because a hold is finished - flushed,
// transcribed, pasted - in the background while the next one records.
type take struct {
	rec   *recording
	timer *time.Timer // opens the connection once the hold outlasts a tap

	mu      sync.Mutex
	dialing bool // a dial was started; dialDone will receive its result
	settled bool // the hold is over; no dial may start any more

	dialDone chan dialResult
}

// settle ends the window in which a dial may start and reports whether one
// did.
func (t *take) settle() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.settled = true
	return t.dialing
}

type Daemon struct {
	recorder *audio.Recorder
	hotkeys  *hotkeys.Manager
	sink     *audioSink
	apiKey   string
	verbose  bool

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

	// cur is the hold in progress. Only OnPress and OnRelease touch it, and
	// they run one after the other on the hotkey goroutine.
	cur *take

	// finished is closed when the most recently queued finish completes.
	// Each finish waits for the one before it, so results are reported and
	// pasted in the order they were spoken.
	finishMu sync.Mutex
	finished chan struct{}

	// discards counts sessions being closed in the background, so shutdown
	// can wait for them.
	discards sync.WaitGroup

	restarting atomic.Bool
	listenEnd  chan struct{}
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
	d.hotkeys = hotkeys.NewManager(d)

	// No session is opened here. AssemblyAI bills the wall-clock time a
	// connection stays open, so a session dialled at startup and left idle
	// until the first recording is billed for the entire wait.

	return nil
}

func (d *Daemon) Run() error {
	if err := d.hotkeys.Start(); err != nil {
		return fmt.Errorf("failed to start hotkey: %v", err)
	}

	c := make(chan os.Signal, 2)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	fmt.Println("🎤 T2 - Voice-to-Text Daemon Started")
	fmt.Printf("📋 Hold %s to record, release to transcribe & paste\n", hotkeys.Display)
	if src := d.hotkeys.Source(); src != "" {
		fmt.Printf("⌨️  Reading the keys through %s\n", src)
	}
	if why := clipboard.Limitation(); why != "" {
		fmt.Printf("⚠️  %s\n", why)
	}
	fmt.Println("🛑 Press Ctrl+C to exit")
	fmt.Println()

	go func() {
		defer close(d.listenEnd)
		d.hotkeys.Listen()
	}()

	<-c
	fmt.Println("\n🛑 Shutting down...")

	cleaned := make(chan struct{})
	go func() {
		d.Cleanup()
		close(cleaned)
	}()
	select {
	case <-cleaned:
	case <-c:
		fmt.Println("🛑 Exiting without waiting")
	}

	return nil
}

// Cleanup stops listening and closes out everything in flight, so that no
// session is left open and billed and nothing already said is lost: a hold
// still in progress is saved to disk, and recordings already released are
// given time to finish and paste.
func (d *Daemon) Cleanup() {
	listening := false
	if d.hotkeys != nil {
		d.hotkeys.Stop()
		select {
		case <-d.listenEnd:
		case <-time.After(shutdownWait):
			listening = true
		}
	}

	// Listen has returned, so the hold in progress, if any, is ours. Listen
	// returns mid-hold without a release, so treat shutdown as one.
	if !listening {
		d.abandonHold()
	}

	if !d.waitIdle(shutdownWait) {
		fmt.Println("⚠️  Gave up waiting for a recording that was still being finished")
	}
	waitGroup(&d.discards, shutdownWait)

	if d.recorder != nil {
		d.recorder.Stop()
	}
}

// abandonHold ends a hold that shutdown interrupted: the audio is saved and
// the session, if one was opened, is closed.
func (d *Daemon) abandonHold() {
	t := d.cur
	if t == nil {
		return
	}
	d.cur = nil

	d.recorder.Stop()
	t.timer.Stop()
	dialed := t.settle()

	fmt.Println("🛑 A recording was in progress")
	d.saveAudio(t.rec)
	t.rec.stop()
	d.discardInBackground(t, dialed)
}

// OnPress implements hotkeys.EventHandler
func (d *Daemon) OnPress() {
	if d.cur != nil {
		return
	}

	// A press while the audio layer is wedged: nothing can be recorded, so
	// there is no point waiting out the restart grace period, and no beep
	// either - a beep player still alive at exec would be left as a zombie.
	// Recordings already released finish first; the exec would take them
	// with it.
	if audio.Wedged() {
		d.waitIdle(shutdownWait)
		d.requestRestart()
		return
	}

	d.beep("start")

	// Capture first, connect later. The handshake costs 60-140ms of TLS plus
	// an HTTP upgrade; if capture waited for it, every recording would lose
	// its opening syllable. Audio buffers in the sink until the session is
	// ready.
	rec := d.sink.start()
	if err := d.recorder.Start(); err != nil {
		rec.stop()
		fmt.Printf("❌ Could not start recording: %v\n", err)
		if errors.Is(err, audio.ErrWedged) {
			d.requestRestart()
			return
		}
		fmt.Println("   Check that this terminal has Microphone permission.")
		fmt.Println()
		return
	}

	// The connection opens once the hold has outlasted a tap. Taps and
	// shortcuts then never open a billed session or send the room's audio
	// anywhere, and nothing is lost: the sink has had the audio since the
	// press.
	t := &take{rec: rec, dialDone: make(chan dialResult, 1)}
	t.timer = time.AfterFunc(quickPressThreshold, func() { d.startDial(t) })
	d.cur = t
}

// OnRelease implements hotkeys.EventHandler
func (d *Daemon) OnRelease(rel hotkeys.Release) {
	t := d.cur
	if t == nil {
		return
	}
	d.cur = nil

	// The beep answers the key release, so it goes before the stop: a wedged
	// stop takes seconds to give up, and that used to sound like no beep.
	d.beep("stop")
	outcome := d.recorder.Stop()

	t.timer.Stop()
	if !rel.Cancelled && rel.Held >= quickPressThreshold {
		// A hold whose press reached OnPress late can end before the timer
		// fires; it still needs its connection.
		d.startDial(t)
	}
	dialed := t.settle()

	// The rest - waiting for the connection, flushing, transcribing,
	// pasting - can take seconds, and happens in the background so the next
	// hold records from the moment the keys go down again.
	d.queue(func() { d.finish(t, rel, outcome, dialed) })
}

// startDial opens the hold's connection, unless one was already started or
// the hold has ended.
func (d *Daemon) startDial(t *take) {
	t.mu.Lock()
	if t.dialing || t.settled {
		t.mu.Unlock()
		return
	}
	t.dialing = true
	t.mu.Unlock()

	go d.connect(t)
}

func (d *Daemon) connect(t *take) {
	started := time.Now()

	s, err := d.dial(d.apiKey, audio.SampleRate)
	if err != nil {
		t.dialDone <- dialResult{err: err}
		return
	}

	// Wait for Begin rather than sleeping a fixed 150ms: it is the actual
	// signal that the session exists and can accept audio. A session the
	// server rejects right after the upgrade ends without beginning, and
	// waiting out the timeout for it only delayed the reason by five seconds.
	select {
	case <-s.Begun():
	case <-s.Done():
		err := s.Err()
		if err == nil {
			err = errors.New("the server closed it before it began")
		}
		t.dialDone <- dialResult{session: s, err: err}
		return
	case <-time.After(beginTimeout):
	}

	t.rec.attach(s)
	d.logf("dial+begin took %v", time.Since(started).Round(time.Millisecond))

	t.dialDone <- dialResult{session: s}
}

// finish turns a released hold into its result.
func (d *Daemon) finish(t *take, rel hotkeys.Release, outcome audio.Outcome, dialed bool) {
	d.conclude(t, rel, outcome, dialed)
	fmt.Println()

	// Whatever happened, the sender must not outlive the recording.
	t.rec.stop()

	if outcome.Wedged {
		// The audio was intact; only the shutdown hung. The recording has
		// been reported - transcript pasted, session closed - so deal with
		// it now.
		d.scheduleRestart()
	}
}

func (d *Daemon) conclude(t *take, rel hotkeys.Release, outcome audio.Outcome, dialed bool) {
	if rel.Capped {
		// The poller ended the hold with the keys still down. Said before the
		// result, on every path, so that a modifier stuck in the down state -
		// which produces exactly this once and then silence - is explained
		// rather than mysterious.
		fmt.Printf("⏱  Hold reached the %v limit - recording stopped. Release the keys, then press again to continue.\n", hotkeys.MaxHold)
	}

	// The skip paths. Nothing is transcribed, but a session that was opened
	// still has to be closed: one left open stays billed until the server's
	// 3-hour cap. None of them saves audio, so a tap cannot overwrite a
	// recording an earlier failure saved.
	switch {
	case rel.Cancelled:
		d.discardInBackground(t, dialed)
		if rel.Held < keepCancelledAfter {
			fmt.Println("⌨️  Shortcut detected - skipped")
			return
		}
		// Too long into the hold to be a shortcut: the user had been
		// talking. Transcribing it could paste into whatever the other key
		// just switched to, so the audio is kept instead.
		fmt.Printf("⌨️  Another key was pressed %v into the recording - stopped without transcribing\n", rel.Held.Round(100*time.Millisecond))
		d.saveAudio(t.rec)
		return

	case rel.Held < quickPressThreshold:
		d.discardInBackground(t, dialed)
		fmt.Printf("⚡ Quick press (%v) - skipped\n", rel.Held.Round(time.Millisecond))
		return

	case outcome.Chunks == 0:
		// A silent room still produces chunks. None at all means the device
		// ran no IO: the wrong input selected, or one that is gone.
		d.discardInBackground(t, dialed)
		fmt.Printf("❌ The microphone delivered no audio - check the input device in %s\n", audio.SoundSettings())
		return

	case outcome.MaxRMS < audio.SilenceThreshold:
		d.discardInBackground(t, dialed)
		fmt.Printf("🔇 No speech detected - skipped (level %.0f, threshold %.0f)\n",
			outcome.MaxRMS, audio.SilenceThreshold)
		return
	}

	if !dialed {
		// Unreachable: OnRelease starts the dial for every hold that gets
		// here. Waiting on dialDone would hang every later recording.
		fmt.Println("❌ No connection was opened for this recording")
		d.saveAudio(t.rec)
		return
	}

	result := <-t.dialDone
	if result.err != nil {
		if result.session != nil {
			// Connected, then rejected: a bad key, no credit, too many
			// sessions. The server's reason is the error.
			result.session.Discard()
			fmt.Printf("❌ AssemblyAI rejected the session: %s\n", errText(result.err))
		} else {
			fmt.Printf("❌ Could not reach AssemblyAI: %s\n", errText(result.err))
		}
		d.saveAudio(t.rec)
		return
	}
	s := result.session

	// Everything captured has to reach the server before Terminate, or the
	// end of the recording is never transcribed. Then the sender is stopped
	// so nothing is written to the socket after Terminate.
	drained := t.rec.flush(flushTimeout)
	t.rec.stop()

	transcript := s.Finish(terminationTimeout)

	d.logf("held %v, %d chunks, %d overflow(s), model %q, audio %.1fs, billed %.1fs",
		rel.Held.Round(time.Millisecond), outcome.Chunks, outcome.Overflows,
		transcript.Model, transcript.AudioSeconds, transcript.SessionSeconds)

	d.report(t.rec, transcript, outcome, rel.Capped, drained)
}

// report pastes the transcript and says plainly what was produced.
//
// The old code printed a green check for any non-empty text, so a truncated
// capture, a dropped connection and an unformatted partial all looked like
// success. Anything short of a clean formatted result now says so, and keeps
// the audio.
func (d *Daemon) report(rec *recording, t transcription.Result, outcome audio.Outcome, capped, drained bool) {
	sendErr := rec.err()

	if t.Text == "" {
		fmt.Println("❌ No transcription received")
		if t.Err != nil {
			fmt.Printf("   %s\n", errText(t.Err))
		}
		if sendErr != nil {
			fmt.Printf("   audio streaming stopped: %s\n", errText(sendErr))
		}
		d.saveAudio(rec)
		return
	}

	// With the hotkey physically down, a synthesised paste keystroke arrives
	// with Ctrl+Shift added and does who knows what in the focused app. A capped
	// hold always has the keys down, so it is copied only. Any other waits
	// for the keys to come up - the user may already be holding them for the
	// next dictation - which also keeps results in the order they were said.
	if capped || !d.waitKeysUp() {
		if err := d.copy(t.Text); err != nil {
			fmt.Printf("❌ Copy failed: %v\n", err)
		} else if capped {
			fmt.Printf("📋 Copied to the clipboard - release the keys, then press %s to paste\n", clipboard.PasteKeys)
		} else {
			fmt.Printf("📋 Copied to the clipboard - the keys were still held, so it was not pasted. Press %s to paste\n", clipboard.PasteKeys)
		}
		fmt.Printf("   %s\n", t.Text)
		return
	}

	// Where the desktop takes no synthetic keystrokes the text is only
	// copied. That is how this desktop works, not a failure, so the result
	// is reported like a paste, in the words that fit.
	delivered := "Pasted"
	if err := d.paste(t.Text); err != nil {
		if !errors.Is(err, clipboard.ErrCopiedOnly) {
			fmt.Printf("❌ Paste failed: %v\n", err)
			fmt.Printf("   Transcript: %s\n", t.Text)
			return
		}
		delivered = "Copied"
	}

	switch {
	case sendErr != nil:
		fmt.Printf("⚠️  %s, but the connection dropped mid-recording - text may be incomplete\n", delivered)
		fmt.Printf("   %s\n", errText(sendErr))
		d.saveAudio(rec)
	case !drained:
		fmt.Printf("⚠️  %s, but the end of the recording did not reach the server in time - text may be incomplete\n", delivered)
		d.saveAudio(rec)
	case outcome.Err != nil:
		fmt.Printf("⚠️  %s, but recording stopped early - text may be incomplete\n", delivered)
		fmt.Printf("   %s\n", errText(outcome.Err))
		d.saveAudio(rec)
	case !t.Complete:
		// Carries the server's close code and reason, which used to disappear
		// into a bare "No transcription received".
		fmt.Printf("⚠️  %s, but the session ended abnormally - text may be incomplete\n", delivered)
		fmt.Printf("   %s\n", errText(t.Err))
		d.saveAudio(rec)
	case outcome.Overflows > 0:
		fmt.Printf("⚠️  %s, but %d audio chunk(s) were dropped - text may have gaps\n", delivered, outcome.Overflows)
	case !t.FromFinal:
		fmt.Printf("⚠️  %s an unformatted draft - the server did not finish in time\n", delivered)
	case delivered == "Copied":
		fmt.Printf("📋 Copied to the clipboard - press %s to paste\n", clipboard.PasteKeys)
		d.removeSavedAudio()
	default:
		fmt.Println("✅ Pasted to active application")
		d.removeSavedAudio()
	}

	// Print the text too, so it survives a clipboard that gets overwritten
	// before the user notices the paste landed somewhere unexpected.
	fmt.Printf("   %s\n", t.Text)
}

// waitKeysUp waits for the hotkey to be let go, and reports whether it was
// within keysUpTimeout.
func (d *Daemon) waitKeysUp() bool {
	deadline := time.Now().Add(keysUpTimeout)
	for hotkeys.HotkeyDown() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return true
}

// errText is an error as printed. Errors can carry text from the server - an
// Error message, a close reason, a proxy's refusal - so they get the same
// cleaning as transcripts before they reach the terminal.
func errText(err error) string {
	return transcription.Sanitize(err.Error())
}

// queue runs fn after every finish queued before it.
func (d *Daemon) queue(fn func()) {
	d.finishMu.Lock()
	prev := d.finished
	done := make(chan struct{})
	d.finished = done
	d.finishMu.Unlock()

	go func() {
		defer close(done)
		if prev != nil {
			<-prev
		}
		fn()
	}()
}

// waitIdle waits until every queued finish has completed, and reports whether
// that happened within timeout.
func (d *Daemon) waitIdle(timeout time.Duration) bool {
	d.finishMu.Lock()
	last := d.finished
	d.finishMu.Unlock()

	if last == nil {
		return true
	}
	select {
	case <-last:
		return true
	case <-time.After(timeout):
		return false
	}
}

// discardInBackground closes the hold's session, if one was opened, without
// holding up the results queued behind it.
func (d *Daemon) discardInBackground(t *take, dialed bool) {
	if !dialed {
		return
	}
	d.discards.Add(1)
	go func() {
		defer d.discards.Done()
		if r := <-t.dialDone; r.session != nil {
			r.session.Discard()
		}
	}()
}

func waitGroup(wg *sync.WaitGroup, timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// saveAudio writes the captured recording to disk, so a failure anywhere
// downstream never costs the user what they already said. The file is the
// user's voice, so it is readable by them only, in a directory only they can
// open, and it is deleted again by the next successful transcription.
func (d *Daemon) saveAudio(rec *recording) {
	pcm := rec.audio()
	if len(pcm) == 0 {
		return
	}

	dir, err := config.EnsureConfigDir()
	if err != nil {
		fmt.Printf("   (could not save audio: %v)\n", err)
		return
	}

	path := filepath.Join(dir, savedAudioName)
	if err := audio.WriteWAV(path, pcm); err != nil {
		fmt.Printf("   (could not save audio: %v)\n", err)
		return
	}

	fmt.Printf("   Audio saved to %s (kept until the next successful transcription)\n", path)
}

func (d *Daemon) removeSavedAudio() {
	dir, err := config.EnsureConfigDir()
	if err != nil {
		return
	}
	if err := os.Remove(filepath.Join(dir, savedAudioName)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		d.logf("could not remove the saved recording: %v", err)
	}
}

// scheduleRestart deals with a wedged audio layer once the recording that hit
// it has been reported. It waits out a grace period first: a stop that was
// merely slow completes in that time and nothing needs to happen, while a
// press during the wait restarts at once (see OnPress).
func (d *Daemon) scheduleRestart() {
	grace := restartGrace
	fmt.Printf("⚠️  The microphone did not shut down (%s hang). Restarting t2 in %v unless it recovers...\n", audio.Backend(), grace)
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
		d.requestRestart()
	}()
}

// requestRestart calls restart unless a call is already under way: a press
// and the end of the grace period can both decide to restart at once.
func (d *Daemon) requestRestart() {
	if !d.restarting.CompareAndSwap(false, true) {
		return
	}
	defer d.restarting.Store(false)
	d.restart()
}

// restartTimesEnv carries the times of recent automatic restarts across the
// exec, as Unix seconds separated by commas, so a restart loop can be noticed.
const restartTimesEnv = "T2_RESTART_TIMES"

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
// It gives up once restartLimit restarts have happened within restartWindow:
// past that the deadlock is not the occasional race it is meant for but
// something broken, and restarting after every recording would only hide it.
func restartProcess() {
	now := time.Now()
	recent := recentRestarts(os.Getenv(restartTimesEnv), now)

	if len(recent) >= restartLimit {
		fmt.Printf("❌ The audio layer deadlocked again - %d restarts in the last hour, so not restarting automatically any more.\n", len(recent))
		fmt.Println("   Please restart t2 by hand, and report this if it keeps happening.")
		fmt.Println()
		return
	}

	recent = append(recent, now.Unix())
	times := make([]string, len(recent))
	for i, t := range recent {
		times[i] = strconv.FormatInt(t, 10)
	}
	if err := os.Setenv(restartTimesEnv, strings.Join(times, ",")); err != nil {
		// Without the marker the new image could not count this restart,
		// and the limit above would never be reached.
		fmt.Printf("❌ Automatic restart skipped: %v\n", err)
		fmt.Println("   Please restart t2 by hand.")
		fmt.Println()
		return
	}

	fmt.Printf("🔁 The audio layer hung inside %s. Restarting t2 automatically...\n", audio.Backend())
	fmt.Println()

	exe, err := os.Executable()
	if err == nil {
		// Exec only returns on failure. Every thread, the stuck one included,
		// dies with the old image; the new process starts clean.
		err = syscall.Exec(exe, restartArgs(os.Args), os.Environ())
	}

	fmt.Printf("❌ Automatic restart failed: %v\n", err)
	fmt.Println("   Please restart t2 by hand.")
	fmt.Println()
}

// recentRestarts parses restartTimesEnv and keeps the restarts that happened
// within restartWindow of now.
func recentRestarts(env string, now time.Time) []int64 {
	var recent []int64
	for _, field := range strings.Split(env, ",") {
		t, err := strconv.ParseInt(strings.TrimSpace(field), 10, 64)
		if err != nil {
			continue
		}
		if now.Sub(time.Unix(t, 0)) <= restartWindow {
			recent = append(recent, t)
		}
	}
	return recent
}

// restartArgs is the command line for the restarted image: the same one,
// minus --reset-key. Replayed, it would delete the key saved minutes earlier
// and leave the new image waiting at a prompt nobody is watching.
func restartArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for i, arg := range args {
		name, _, _ := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if i > 0 && strings.HasPrefix(arg, "-") && name == "reset-key" {
			continue
		}
		out = append(out, arg)
	}
	return out
}

func (d *Daemon) logf(format string, args ...any) {
	if !d.verbose {
		return
	}
	fmt.Printf("   [v] "+format+"\n", args...)
}
