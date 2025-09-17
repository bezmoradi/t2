package metrics

import (
	"strings"
	"time"
)

type SessionMetrics struct {
	Timestamp     time.Time     `json:"timestamp"`
	WordCount     int           `json:"word_count"`
	RecordingTime time.Duration `json:"recording_time"`
	TimeSaved     time.Duration `json:"time_saved"`
	SpeakingRate  int           `json:"speaking_rate"` // WPM
}

type DailyMetrics struct {
	Date         string           `json:"date"`
	Sessions     []SessionMetrics `json:"sessions"`
	TotalWords   int              `json:"total_words"`
	TotalSaved   time.Duration    `json:"total_saved"`
	SessionCount int              `json:"session_count"`
}

type UserSettings struct {
	TypingSpeed int `json:"typing_speed"` // User's actual WPM for personalized calculations
}

type MetricsManager struct {
	storage      *Storage
	userSettings *UserSettings
}

func NewMetricsManager(storagePath string) (*MetricsManager, error) {
	storage, err := NewStorage(storagePath)
	if err != nil {
		return nil, err
	}

	userSettings, err := storage.LoadUserSettings()
	if err != nil {
		// Use default typing speed if no settings found
		userSettings = &UserSettings{
			TypingSpeed: 40, // Default average typing speed
		}
	}

	return &MetricsManager{
		storage:      storage,
		userSettings: userSettings,
	}, nil
}

func (mm *MetricsManager) RecordSession(transcript string, recordingTime time.Duration) (*SessionMetrics, error) {
	wordCount := countWords(transcript)
	speakingRate := calculateSpeakingRate(wordCount, recordingTime)
	timeSaved := mm.calculateTimeSaved(wordCount, recordingTime)

	session := &SessionMetrics{
		Timestamp:     time.Now(),
		WordCount:     wordCount,
		RecordingTime: recordingTime,
		TimeSaved:     timeSaved,
		SpeakingRate:  speakingRate,
	}

	if err := mm.storage.SaveSession(session); err != nil {
		return session, err
	}

	return session, nil
}

func (mm *MetricsManager) GetTodayMetrics() (*DailyMetrics, error) {
	today := time.Now().Format("2006-01-02")
	return mm.storage.GetDailyMetrics(today)
}

func (mm *MetricsManager) GetTotalMetrics() (*TotalMetrics, error) {
	return mm.storage.GetTotalMetrics()
}

func (mm *MetricsManager) SetTypingSpeed(wpm int) error {
	mm.userSettings.TypingSpeed = wpm
	return mm.storage.SaveUserSettings(mm.userSettings)
}

func (mm *MetricsManager) GetTypingSpeed() int {
	return mm.userSettings.TypingSpeed
}

func (mm *MetricsManager) GetRecentDays(days int) ([]*DailyMetrics, error) {
	return mm.storage.GetRecentDays(days)
}

func (mm *MetricsManager) ClearAllMetrics() error {
	return mm.storage.ClearAllMetrics()
}

func (mm *MetricsManager) calculateTimeSaved(wordCount int, recordingTime time.Duration) time.Duration {
	if wordCount == 0 {
		return 0
	}

	// Calculate time it would take to type these words
	typingTimeMinutes := float64(wordCount) / float64(mm.userSettings.TypingSpeed)
	typingTime := time.Duration(typingTimeMinutes * float64(time.Minute))

	// Time saved = typing time - recording time
	timeSaved := typingTime - recordingTime
	return max(timeSaved, 0)
}

func countWords(text string) int {
	if text == "" {
		return 0
	}

	fields := strings.Fields(strings.TrimSpace(text))
	return len(fields)
}

func calculateSpeakingRate(wordCount int, duration time.Duration) int {
	if duration == 0 {
		return 0
	}

	minutes := duration.Minutes()
	if minutes == 0 {
		return 0
	}

	return int(float64(wordCount) / minutes)
}

type TotalMetrics struct {
	TotalWords         int           `json:"total_words"`
	TotalSessions      int           `json:"total_sessions"`
	TotalSaved         time.Duration `json:"total_saved"`
	AvgWordsPerSession int           `json:"avg_words_per_session"`
	AvgSavedPerSession time.Duration `json:"avg_saved_per_session"`
}
