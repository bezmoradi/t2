package app

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bezmoradi/t2/internal/audio"
	"github.com/bezmoradi/t2/internal/clipboard"
	"github.com/bezmoradi/t2/internal/config"
	"github.com/bezmoradi/t2/internal/hotkeys"
	"github.com/bezmoradi/t2/internal/metrics"
	"github.com/bezmoradi/t2/internal/terminal"
	"github.com/bezmoradi/t2/internal/transcription"
)

type Daemon struct {
	config             *config.Config
	recorder           *audio.Recorder
	transcriptClient   *transcription.Client
	processor          *transcription.Processor
	hotkeyManager      *hotkeys.Manager
	metricsManager     *metrics.MetricsManager
	terminalControl    *terminal.Control
	apiKey             string
	currentTurnOrder   int
	sessionStartTime   time.Time
	isFirstSession     bool
	pressTime          time.Time
	quickPressThreshold time.Duration
}

func NewDaemon() *Daemon {
	return &Daemon{
		isFirstSession:      true,
		quickPressThreshold: 800 * time.Millisecond,
	}
}

func (d *Daemon) Initialize() error {
	// Get API key using fallback priority system
	var err error
	d.apiKey, err = config.GetAPIKey()
	if err != nil {
		return fmt.Errorf("failed to get AssemblyAI API key: %v", err)
	}

	// Load configuration
	d.config, err = config.LoadConfig()
	if err != nil {
		d.config = &config.Config{}
	}

	// Initialize processor
	d.processor = transcription.NewProcessor()

	// Initialize transcription client
	d.transcriptClient = transcription.NewClient(
		d.handleTranscript,
		d.handleConnection,
	)
	d.transcriptClient.SetTerminationCallback(d.handleTermination)

	// Initialize recorder with audio callback
	d.recorder = audio.NewRecorder(d.transcriptClient.SendAudio)

	// Silence detection is now handled on key release instead of real-time callback
	// d.recorder.SetSilenceCallback(d.handleSilenceDetected)

	// Initialize hotkey manager
	d.hotkeyManager = hotkeys.NewManager(d)

	// Initialize metrics manager
	metricsDir, err := config.GetMetricsDir()
	if err != nil {
		return fmt.Errorf("failed to get metrics directory: %v", err)
	}
	d.metricsManager, err = metrics.NewMetricsManager(metricsDir)
	if err != nil {
		return fmt.Errorf("failed to initialize metrics manager: %v", err)
	}

	// Initialize terminal control
	d.terminalControl = terminal.NewControl()

	// Initialize PortAudio
	if err := audio.Initialize(); err != nil {
		return fmt.Errorf("failed to initialize PortAudio: %v", err)
	}

	// Connect to AssemblyAI
	if err := d.transcriptClient.Connect(d.apiKey); err != nil {
		return fmt.Errorf("failed to connect to AssemblyAI streaming API: %v", err)
	}

	return nil
}

func (d *Daemon) Run() error {
	if err := d.hotkeyManager.Start(); err != nil {
		return fmt.Errorf("failed to start hotkey: %v", err)
	}

	// Setup graceful shutdown
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	fmt.Println("🎤 T2 - Voice-to-Text Daemon Started")
	fmt.Printf("📋 Hold %s to record, release to transcribe & paste\n", d.hotkeyManager.GetHotkeyDisplay())
	fmt.Println("🛑 Press Ctrl+C to exit")
	fmt.Println()

	// Start hotkey listening in a goroutine
	go d.hotkeyManager.Listen()

	// Wait for shutdown signal
	<-c
	fmt.Println("\n🛑 Shutting down...")
	d.Cleanup()
	return nil
}

func (d *Daemon) Cleanup() {
	// Stop hotkey manager
	if d.hotkeyManager != nil {
		d.hotkeyManager.Stop()
	}

	// Stop recording if still running
	if d.recorder != nil {
		d.recorder.Stop()
	}

	// Close transcription client
	if d.transcriptClient != nil {
		d.transcriptClient.Close()
	}

	// Terminate PortAudio
	audio.Terminate()
}

// OnPress implements hotkeys.EventHandler
func (d *Daemon) OnPress() {

	// Check if already recording to prevent overlapping sessions
	if d.recorder.IsRecording() {
		return
	}

	// Check if connection needs refresh due to degradation
	if d.transcriptClient.ConnectionNeedsRefresh() {
		d.transcriptClient.Close()
		time.Sleep(100 * time.Millisecond)
	}

	// Silently reconnect if needed (happens after Terminate closes the connection)
	if !d.transcriptClient.IsConnected() {
		if err := d.transcriptClient.Connect(d.apiKey); err != nil {
			fmt.Printf("❌ Connection failed: %v\n", err)
			d.transcriptClient.ReportSessionFailure()
			return
		}
		// Brief pause to let connection establish
		time.Sleep(150 * time.Millisecond)
	}

	audio.PlayBeep("start")

	// Reset processor for new recording
	d.processor.Reset()
	d.currentTurnOrder = 0

	// Record press time for quick-press detection (just before starting recording)
	d.pressTime = time.Now()

	// Record session start time for metrics
	d.sessionStartTime = time.Now()

	d.recorder.Start()
}

// OnRelease implements hotkeys.EventHandler
func (d *Daemon) OnRelease() {

	// Check if we're actually recording
	if !d.recorder.IsRecording() {
		return
	}

	// Calculate recording duration for quick-press detection
	recordingDuration := time.Since(d.pressTime)

	d.recorder.Stop()
	audio.PlayBeep("stop")

	// Layer 1: Check for quick press - skip transcription if too short
	if recordingDuration < d.quickPressThreshold {
		fmt.Println("⚡ Quick press detected - skipped")
		fmt.Println()
		return
	}

	// Layer 2: Check for prolonged silence or low audio levels
	maxRMS := d.recorder.GetMaxRMS()
	hadProlongedSilence := d.recorder.HasProlongedSilence()

	// Skip if we had prolonged silence without any significant speech
	if hadProlongedSilence && maxRMS < 150.0 {
		fmt.Println("🔇 Real-time silence detected - skipped")
		fmt.Println()
		// Reset processor to discard any accumulated audio from this session
		d.processor.Reset()
		return
	}

	// Also check traditional silence detection for very quiet recordings
	if !hadProlongedSilence && maxRMS < 150.0 {
		fmt.Println("🔇 No speech detected - skipped")
		fmt.Println()
		// Reset processor to discard any accumulated audio from this session
		d.processor.Reset()
		return
	}

	// Immediate termination for true streaming - send termination right away
	d.transcriptClient.Terminate()

	terminationTimeout := 1 * time.Second // Balanced timeout for reliability + UX
	select {
	case <-d.processor.WaitForTermination():
	case <-time.After(terminationTimeout):
	}

	// Get the final transcript or fallback to best partial
	text, _ := d.processor.ConsumeTranscriptWithFallback()

	// Guarantee clean state for next session (prevents cross-session contamination)
	d.processor.Reset()

	if text != "" {
		if err := clipboard.PasteTextSafely(text); err != nil {
			fmt.Printf("❌ Paste failed: %v\n", err)
		} else {
			// Record metrics and display enhanced output
			d.displaySessionMetrics(text)
			// Report successful session to improve connection health
			d.transcriptClient.ReportSessionSuccess()
		}
	} else {
		fmt.Println("❌ No transcription received")
		// Report failed session to degrade connection health
		d.transcriptClient.ReportSessionFailure()
	}
	fmt.Println()
}

// handleTranscript handles incoming transcripts from the transcription client
func (d *Daemon) handleTranscript(transcript string, isComplete bool, endOfTurn bool, confidence float64) {
	// AssemblyAI sends progressive partial transcripts that already contain
	// the accumulated text, so we use the same turn order (0) for all partials
	// and only mark completion when we get the final formatted transcript

	turnOrder := 0
	d.processor.ProcessTranscript(transcript, turnOrder, isComplete, endOfTurn, confidence)
}

// handleConnection handles connection status changes
func (d *Daemon) handleConnection(connected bool) {
	// Connection status changes are handled silently
	// Audio beeps provide user feedback instead
}

// handleTermination handles session termination from AssemblyAI
func (d *Daemon) handleTermination() {
	d.processor.SignalTermination()
}

// handleSilenceDetected handles real-time silence detection from audio recorder
func (d *Daemon) handleSilenceDetected() {
	log.Printf("[SESSION] Real-time silence detected by audio recorder")

	// Check if we're actually recording to prevent race conditions
	if !d.recorder.IsRecording() {
		log.Printf("[SESSION] Silence detected but not recording, ignoring")
		return
	}

	// Stop recording immediately
	log.Printf("[SESSION] Stopping recording due to real-time silence detection")
	d.recorder.Stop()
	audio.PlayBeep("stop")

	// Log the session as skipped due to silence
	log.Printf("[SESSION] Real-time silence skipped")
	fmt.Println("🔇 Real-time silence detected - skipped")
	fmt.Println()
	log.Printf("[SESSION] ===== SESSION COMPLETE =====")
}

func (d *Daemon) displaySessionMetrics(text string) {
	// Calculate recording duration
	recordingDuration := time.Since(d.sessionStartTime)

	// Record session metrics
	sessionMetrics, err := d.metricsManager.RecordSession(text, recordingDuration)
	if err != nil {
		fmt.Printf("⚠️  Warning: Failed to record session metrics: %v\n", err)
		fmt.Println("✅ Pasted to active application")
		return
	}

	// Get today's metrics for cumulative display
	todayMetrics, err := d.metricsManager.GetTodayMetrics()
	if err != nil {
		fmt.Printf("⚠️  Warning: Failed to get today's metrics: %v\n", err)
		todayMetrics = nil
	}

	// Format and display the enhanced output with dynamic updates
	formatter := metrics.NewStatsFormatter()
	lines := formatter.FormatSessionSummaryLines(sessionMetrics, todayMetrics)

	// Use terminal control for dynamic updates
	d.terminalControl.UpdateInPlace(lines, d.isFirstSession)

	// Mark that we've had our first session
	d.isFirstSession = false
}
