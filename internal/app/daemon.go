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
	quickPressThreshold = 400 * time.Millisecond
	shortcutMaxHold     = 3 * time.Second
	terminationTimeout  = 4 * time.Second
	beginTimeout        = 5 * time.Second
	shutdownWait        = 15 * time.Second

	restartLimit  = 3
	restartWindow = time.Hour
	restartGrace  = 10 * time.Second

	flushTimeout       = 3 * time.Second
	keysUpTimeout      = hotkeys.MaxHold
	keysUpPollInterval = 20 * time.Millisecond
	wedgePollInterval  = 200 * time.Millisecond

	savedAudioName = "last-recording.wav"
)

type dialResult struct {
	openedSession *transcription.Session
	err           error
}

type hold struct {
	rec       *recording
	dialTimer *time.Timer

	mu               sync.Mutex
	dialStarted      bool
	dialWindowClosed bool

	dialDone chan dialResult
}

func (h *hold) closeDialWindow() (dialStarted bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dialWindowClosed = true
	return h.dialStarted
}

type Daemon struct {
	recorder *audio.Recorder
	hotkeys  *hotkeys.Manager
	sink     *audioSink
	apiKey   string
	verbose  bool

	activeHold *hold

	finishMu           sync.Mutex
	lastFinishDone     chan struct{}
	backgroundDiscards sync.WaitGroup

	restarting atomic.Bool
	listenEnd  chan struct{}
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
	d.hotkeys = hotkeys.NewManager(d)

	return nil
}

func (d *Daemon) Run() error {
	var warnings []string
	for _, why := range []string{audio.Limitation(), clipboard.Limitation()} {
		if why != "" {
			warnings = append(warnings, why)
		}
	}
	printWarnings := func() {
		for _, why := range warnings {
			fmt.Printf("⚠️  %s\n", why)
		}
	}

	if err := d.hotkeys.Start(); err != nil {
		printWarnings()
		return fmt.Errorf("failed to start hotkey: %v", err)
	}

	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	fmt.Println("🎤 T2 - Voice-to-Text Daemon Started")
	fmt.Printf("📋 Hold %s to record, release to transcribe & paste\n", hotkeys.Display)
	if src := d.hotkeys.Source(); src != "" {
		fmt.Printf("⌨️  Reading the keys through %s\n", src)
	}
	printWarnings()
	fmt.Println("🛑 Press Ctrl+C to exit")
	fmt.Println()

	go func() {
		defer close(d.listenEnd)
		d.hotkeys.Listen()
	}()

	<-signals
	fmt.Println("\n🛑 Shutting down...")

	cleaned := make(chan struct{})
	go func() {
		d.Cleanup()
		close(cleaned)
	}()
	select {
	case <-cleaned:
	case <-signals:
		fmt.Println("🛑 Exiting without waiting")
	}

	return nil
}

func (d *Daemon) Cleanup() {
	stillListening := false
	if d.hotkeys != nil {
		d.hotkeys.Stop()
		select {
		case <-d.listenEnd:
		case <-time.After(shutdownWait):
			stillListening = true
		}
	}

	if !stillListening {
		d.saveHoldInterruptedByShutdown()
	}

	if !d.waitIdle(shutdownWait) {
		fmt.Println("⚠️  Gave up waiting for a recording that was still being finished")
	}
	waitWithTimeout(&d.backgroundDiscards, shutdownWait)

	if d.recorder != nil {
		d.recorder.Stop()
	}
}

func (d *Daemon) saveHoldInterruptedByShutdown() {
	h := d.activeHold
	if h == nil {
		return
	}
	d.activeHold = nil

	d.recorder.Stop()
	h.dialTimer.Stop()
	dialStarted := h.closeDialWindow()

	fmt.Println("🛑 A recording was in progress")
	d.saveAudio(h.rec)
	h.rec.stop()
	d.discardInBackground(h, dialStarted)
}

func (d *Daemon) OnPress() {
	if d.activeHold != nil {
		return
	}

	if audio.Wedged() {
		d.restartOnceIdle()
		return
	}

	audio.PlayBeep("start")

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

	h := &hold{rec: rec, dialDone: make(chan dialResult, 1)}
	h.dialTimer = time.AfterFunc(quickPressThreshold, func() { d.startDial(h) })
	d.activeHold = h
}

func (d *Daemon) restartOnceIdle() {
	d.waitIdle(shutdownWait)
	d.requestRestart()
}

func (d *Daemon) OnRelease(rel hotkeys.Release) {
	h := d.activeHold
	if h == nil {
		return
	}
	d.activeHold = nil

	audio.PlayBeep("stop")
	outcome := d.recorder.Stop()

	h.dialTimer.Stop()
	longEnoughToTranscribe := !rel.CancelledByOtherKey && rel.HeldFor >= quickPressThreshold
	if longEnoughToTranscribe {
		d.startDial(h)
	}
	dialStarted := h.closeDialWindow()

	d.runInOrder(func() { d.finish(h, rel, outcome, dialStarted) })
}

func (d *Daemon) startDial(h *hold) {
	h.mu.Lock()
	if h.dialStarted || h.dialWindowClosed {
		h.mu.Unlock()
		return
	}
	h.dialStarted = true
	h.mu.Unlock()

	go d.connect(h)
}

func (d *Daemon) connect(h *hold) {
	started := time.Now()

	s, err := transcription.Dial(d.apiKey, audio.SampleRate)
	if err != nil {
		h.dialDone <- dialResult{err: err}
		return
	}

	select {
	case <-s.Begun():
	case <-s.Done():
		err := s.Err()
		if err == nil {
			err = errors.New("the server closed it before it began")
		}
		h.dialDone <- dialResult{openedSession: s, err: err}
		return
	case <-time.After(beginTimeout):
	}

	h.rec.attach(s)
	d.logf("dial+begin took %v", time.Since(started).Round(time.Millisecond))

	h.dialDone <- dialResult{openedSession: s}
}

func (d *Daemon) finish(h *hold, rel hotkeys.Release, outcome audio.Outcome, dialStarted bool) {
	d.conclude(h, rel, outcome, dialStarted)
	fmt.Println()

	h.rec.stop()

	if outcome.Wedged {
		d.restartUnlessRecovered()
	}
}

func (d *Daemon) conclude(h *hold, rel hotkeys.Release, outcome audio.Outcome, dialStarted bool) {
	if rel.CappedAtMaxHold {
		fmt.Printf("⏱  Hold reached the %v limit - recording stopped. Release the keys, then press again to continue.\n", hotkeys.MaxHold)
	}

	switch {
	case rel.CancelledByOtherKey:
		d.discardInBackground(h, dialStarted)
		if rel.HeldFor < shortcutMaxHold {
			fmt.Println("⌨️  Shortcut detected - skipped")
			return
		}
		fmt.Printf("⌨️  Another key was pressed %v into the recording - stopped without transcribing\n", rel.HeldFor.Round(100*time.Millisecond))
		d.saveAudio(h.rec)
		return

	case rel.HeldFor < quickPressThreshold:
		d.discardInBackground(h, dialStarted)
		fmt.Printf("⚡ Quick press (%v) - skipped\n", rel.HeldFor.Round(time.Millisecond))
		return

	case outcome.Chunks == 0:
		d.discardInBackground(h, dialStarted)
		fmt.Printf("❌ The microphone delivered no audio - check the input device in %s\n", audio.SoundSettings())
		return

	case outcome.MaxRMS < audio.SilenceThreshold:
		d.discardInBackground(h, dialStarted)
		fmt.Printf("🔇 No speech detected - skipped (level %.0f, threshold %.0f)\n",
			outcome.MaxRMS, audio.SilenceThreshold)
		return
	}

	if !dialStarted {
		fmt.Println("❌ No connection was opened for this recording")
		d.saveAudio(h.rec)
		return
	}

	result := <-h.dialDone
	if result.err != nil {
		if result.openedSession != nil {
			result.openedSession.Discard()
			fmt.Printf("❌ AssemblyAI rejected the session: %s\n", errText(result.err))
		} else {
			fmt.Printf("❌ Could not reach AssemblyAI: %s\n", errText(result.err))
		}
		d.saveAudio(h.rec)
		return
	}
	s := result.openedSession

	allAudioSent := h.rec.sendRestThenStop(flushTimeout)

	transcript := s.Finish(terminationTimeout)

	d.logf("held %v, %d chunks, %d overflow(s), model %q, audio %.1fs, billed %.1fs",
		rel.HeldFor.Round(time.Millisecond), outcome.Chunks, outcome.Overflows,
		transcript.Model, transcript.AudioSeconds, transcript.BilledSeconds)

	d.report(h.rec, transcript, outcome, rel.CappedAtMaxHold, allAudioSent)
}

func (d *Daemon) report(rec *recording, transcript transcription.Result, outcome audio.Outcome, capped, allAudioSent bool) {
	sendErr := rec.err()

	if transcript.Text == "" {
		fmt.Println("❌ No transcription received")
		if transcript.Err != nil {
			fmt.Printf("   %s\n", errText(transcript.Err))
		}
		if sendErr != nil {
			fmt.Printf("   audio streaming stopped: %s\n", errText(sendErr))
		}
		d.saveAudio(rec)
		return
	}

	keysStillHeld := capped || !d.waitKeysUp()
	if keysStillHeld {
		if err := clipboard.CopyText(transcript.Text); err != nil {
			fmt.Printf("❌ Copy failed: %v\n", err)
		} else if capped {
			fmt.Printf("📋 Copied to the clipboard - release the keys, then press %s to paste\n", clipboard.PasteKeys)
		} else {
			fmt.Printf("📋 Copied to the clipboard - the keys were still held, so it was not pasted. Press %s to paste\n", clipboard.PasteKeys)
		}
		fmt.Printf("   %s\n", transcript.Text)
		return
	}

	delivered := "Pasted"
	if err := clipboard.PasteOnce(transcript.Text); err != nil {
		if !errors.Is(err, clipboard.ErrCopiedOnly) {
			fmt.Printf("❌ Paste failed: %v\n", err)
			fmt.Printf("   Transcript: %s\n", transcript.Text)
			return
		}
		delivered = "Copied"
	}

	switch {
	case sendErr != nil:
		fmt.Printf("⚠️  %s, but the connection dropped mid-recording - text may be incomplete\n", delivered)
		fmt.Printf("   %s\n", errText(sendErr))
		d.saveAudio(rec)
	case !allAudioSent:
		fmt.Printf("⚠️  %s, but the end of the recording did not reach the server in time - text may be incomplete\n", delivered)
		d.saveAudio(rec)
	case outcome.Err != nil:
		fmt.Printf("⚠️  %s, but recording stopped early - text may be incomplete\n", delivered)
		fmt.Printf("   %s\n", errText(outcome.Err))
		d.saveAudio(rec)
	case !transcript.EndedCleanly:
		fmt.Printf("⚠️  %s, but the session ended abnormally - text may be incomplete\n", delivered)
		fmt.Printf("   %s\n", errText(transcript.Err))
		d.saveAudio(rec)
	case outcome.Overflows > 0:
		fmt.Printf("⚠️  %s, but %d audio chunk(s) were dropped - text may have gaps\n", delivered, outcome.Overflows)
	case !transcript.LastTurnFormatted:
		fmt.Printf("⚠️  %s an unformatted draft - the server did not finish in time\n", delivered)
	case delivered == "Copied":
		fmt.Printf("📋 Copied to the clipboard - press %s to paste\n", clipboard.PasteKeys)
		d.removeSavedAudio()
	default:
		fmt.Println("✅ Pasted to active application")
		d.removeSavedAudio()
	}

	fmt.Printf("   %s\n", transcript.Text)
}

func (d *Daemon) waitKeysUp() (releasedInTime bool) {
	deadline := time.Now().Add(keysUpTimeout)
	for hotkeys.HotkeyDown() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(keysUpPollInterval)
	}
	return true
}

func errText(err error) string {
	return transcription.Sanitize(err.Error())
}

func (d *Daemon) runInOrder(fn func()) {
	d.finishMu.Lock()
	previousDone := d.lastFinishDone
	done := make(chan struct{})
	d.lastFinishDone = done
	d.finishMu.Unlock()

	go func() {
		defer close(done)
		if previousDone != nil {
			<-previousDone
		}
		fn()
	}()
}

func (d *Daemon) waitIdle(timeout time.Duration) (idleInTime bool) {
	d.finishMu.Lock()
	last := d.lastFinishDone
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

func (d *Daemon) discardInBackground(h *hold, dialStarted bool) {
	if !dialStarted {
		return
	}
	d.backgroundDiscards.Add(1)
	go func() {
		defer d.backgroundDiscards.Done()
		if r := <-h.dialDone; r.openedSession != nil {
			r.openedSession.Discard()
		}
	}()
}

func waitWithTimeout(wg *sync.WaitGroup, timeout time.Duration) {
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

func (d *Daemon) restartUnlessRecovered() {
	fmt.Printf("⚠️  The microphone did not shut down (%s hang). Restarting t2 in %v unless it recovers...\n", audio.Backend(), restartGrace)
	fmt.Println()

	go func() {
		deadline := time.Now().Add(restartGrace)
		for time.Now().Before(deadline) {
			if !audio.Wedged() {
				fmt.Println("✅ The microphone shut down after all - no restart needed")
				fmt.Println()
				return
			}
			time.Sleep(wedgePollInterval)
		}
		d.requestRestart()
	}()
}

func (d *Daemon) requestRestart() {
	alreadyRestarting := !d.restarting.CompareAndSwap(false, true)
	if alreadyRestarting {
		return
	}
	defer d.restarting.Store(false)
	restartProcess()
}

const restartTimesEnv = "T2_RESTART_TIMES"

func restartProcess() {
	now := time.Now()
	recent := recentRestarts(os.Getenv(restartTimesEnv), now)

	if len(recent) >= restartLimit {
		fmt.Printf("❌ The audio layer deadlocked again - %d restarts in the last hour, so not restarting automatically any more.\n", len(recent))
		fmt.Println("   Please restart t2 by hand, and report this if it keeps happening.")
		fmt.Println()
		return
	}

	if err := recordRestartForNextImage(append(recent, now.Unix())); err != nil {
		fmt.Printf("❌ Automatic restart skipped: %v\n", err)
		fmt.Println("   Please restart t2 by hand.")
		fmt.Println()
		return
	}

	fmt.Printf("🔁 The audio layer hung inside %s. Restarting t2 automatically...\n", audio.Backend())
	fmt.Println()

	exe, err := os.Executable()
	if err == nil {
		err = syscall.Exec(exe, argsWithoutResetKey(os.Args), os.Environ())
	}

	fmt.Printf("❌ Automatic restart failed: %v\n", err)
	fmt.Println("   Please restart t2 by hand.")
	fmt.Println()
}

func recordRestartForNextImage(unixTimes []int64) error {
	fields := make([]string, len(unixTimes))
	for i, t := range unixTimes {
		fields[i] = strconv.FormatInt(t, 10)
	}
	return os.Setenv(restartTimesEnv, strings.Join(fields, ","))
}

func recentRestarts(commaSeparatedUnixTimes string, now time.Time) []int64 {
	var recent []int64
	for _, field := range strings.Split(commaSeparatedUnixTimes, ",") {
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

func argsWithoutResetKey(args []string) []string {
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
