package metrics

import (
	"fmt"
	"time"
)

type TimeFormatter struct{}

func NewTimeFormatter() *TimeFormatter {
	return &TimeFormatter{}
}

func (tf *TimeFormatter) FormatDuration(duration time.Duration) string {
	if duration == 0 {
		return "0 seconds"
	}

	hours := int(duration.Hours())
	minutes := int(duration.Minutes()) % 60
	seconds := int(duration.Seconds()) % 60

	if hours > 0 {
		if minutes > 0 {
			return fmt.Sprintf("%d hours %d minutes", hours, minutes)
		}
		return fmt.Sprintf("%d hours", hours)
	}

	if minutes > 0 {
		if seconds > 0 {
			return fmt.Sprintf("%d minutes %d seconds", minutes, seconds)
		}
		return fmt.Sprintf("%d minutes", minutes)
	}

	return fmt.Sprintf("%d seconds", seconds)
}

func (tf *TimeFormatter) FormatDurationShort(duration time.Duration) string {
	if duration == 0 {
		return "0s"
	}

	hours := int(duration.Hours())
	minutes := int(duration.Minutes()) % 60
	seconds := int(duration.Seconds()) % 60

	if hours > 0 {
		if minutes > 0 {
			return fmt.Sprintf("%dh %dm", hours, minutes)
		}
		return fmt.Sprintf("%dh", hours)
	}

	if minutes > 0 {
		if seconds > 0 {
			return fmt.Sprintf("%dm %ds", minutes, seconds)
		}
		return fmt.Sprintf("%dm", minutes)
	}

	return fmt.Sprintf("%ds", seconds)
}

type ProductivityCalculator struct {
	defaultTypingSpeed int
}

func NewProductivityCalculator() *ProductivityCalculator {
	return &ProductivityCalculator{
		defaultTypingSpeed: 40, // Average typing speed
	}
}

func (pc *ProductivityCalculator) CalculateTimeSaved(wordCount int, recordingTime time.Duration, userTypingSpeed int) time.Duration {
	if wordCount == 0 {
		return 0
	}

	typingSpeed := userTypingSpeed
	if typingSpeed <= 0 {
		typingSpeed = pc.defaultTypingSpeed
	}

	// Calculate time it would take to type these words
	typingTimeMinutes := float64(wordCount) / float64(typingSpeed)
	typingTime := time.Duration(typingTimeMinutes * float64(time.Minute))

	// Time saved = typing time - recording time
	timeSaved := typingTime - recordingTime
	if timeSaved < 0 {
		timeSaved = 0
	}

	return timeSaved
}

func (pc *ProductivityCalculator) CalculateEfficiencyPercentage(timeSaved time.Duration, recordingTime time.Duration) float64 {
	if recordingTime == 0 {
		return 0
	}

	totalTimeWouldTake := timeSaved + recordingTime
	if totalTimeWouldTake == 0 {
		return 0
	}

	efficiency := (float64(timeSaved) / float64(totalTimeWouldTake)) * 100
	return efficiency
}

func (pc *ProductivityCalculator) EstimateTypingTime(wordCount int, typingSpeed int) time.Duration {
	if wordCount == 0 || typingSpeed <= 0 {
		return 0
	}

	minutes := float64(wordCount) / float64(typingSpeed)
	return time.Duration(minutes * float64(time.Minute))
}

func (pc *ProductivityCalculator) GetProductivityInsight(totalSaved time.Duration, totalSessions int) string {
	if totalSessions == 0 {
		return "Start using T2 to see your productivity gains!"
	}

	avgSavedPerSession := totalSaved / time.Duration(totalSessions)
	formatter := NewTimeFormatter()

	if totalSaved < time.Minute {
		return fmt.Sprintf("You're saving %s per session on average.", formatter.FormatDurationShort(avgSavedPerSession))
	}

	if totalSaved < time.Hour {
		return fmt.Sprintf("You've saved %s total, averaging %s per session!",
			formatter.FormatDurationShort(totalSaved),
			formatter.FormatDurationShort(avgSavedPerSession))
	}

	return fmt.Sprintf("Amazing! You've saved %s total - that's %s per session on average!",
		formatter.FormatDuration(totalSaved),
		formatter.FormatDurationShort(avgSavedPerSession))
}

type StatsFormatter struct {
	timeFormatter *TimeFormatter
}

func NewStatsFormatter() *StatsFormatter {
	return &StatsFormatter{
		timeFormatter: NewTimeFormatter(),
	}
}

func (sf *StatsFormatter) FormatSessionSummary(session *SessionMetrics, todayMetrics *DailyMetrics) string {
	timeSavedStr := sf.timeFormatter.FormatDurationShort(session.TimeSaved)

	summary := fmt.Sprintf("✅ Pasted %d words (%s recording)\n",
		session.WordCount,
		sf.timeFormatter.FormatDurationShort(session.RecordingTime))

	if session.TimeSaved > 0 {
		summary += fmt.Sprintf("💡 Saved %s vs typing\n", timeSavedStr)
	}

	if session.SpeakingRate > 0 {
		summary += fmt.Sprintf("📊 Session: %d WPM speaking rate\n", session.SpeakingRate)
	}

	if todayMetrics != nil && todayMetrics.SessionCount > 0 {
		todayTimeSaved := sf.timeFormatter.FormatDurationShort(todayMetrics.TotalSaved)
		summary += fmt.Sprintf("📈 Today: %d words, %s saved", todayMetrics.TotalWords, todayTimeSaved)
	}

	return summary
}

func (sf *StatsFormatter) FormatSessionSummaryLines(session *SessionMetrics, todayMetrics *DailyMetrics) []string {
	timeSavedStr := sf.timeFormatter.FormatDurationShort(session.TimeSaved)

	lines := []string{
		fmt.Sprintf("✅ Pasted %d words (%s recording)",
			session.WordCount,
			sf.timeFormatter.FormatDurationShort(session.RecordingTime)),
	}

	if session.TimeSaved > 0 {
		lines = append(lines, fmt.Sprintf("💡 Saved %s vs typing", timeSavedStr))
	}

	if session.SpeakingRate > 0 {
		lines = append(lines, fmt.Sprintf("📊 Session: %d WPM speaking rate", session.SpeakingRate))
	}

	if todayMetrics != nil && todayMetrics.SessionCount > 0 {
		todayTimeSaved := sf.timeFormatter.FormatDurationShort(todayMetrics.TotalSaved)
		lines = append(lines, fmt.Sprintf("📈 Today: %d words, %s saved", todayMetrics.TotalWords, todayTimeSaved))
	}

	return lines
}

func (sf *StatsFormatter) FormatTotalStats(totalMetrics *TotalMetrics) string {
	if totalMetrics.TotalSessions == 0 {
		return "📊 No usage statistics yet. Start using T2 to track your productivity!"
	}

	stats := "📊 Total Statistics:\n"
	stats += fmt.Sprintf("   Words transcribed: %d\n", totalMetrics.TotalWords)
	stats += fmt.Sprintf("   Sessions completed: %d\n", totalMetrics.TotalSessions)
	stats += fmt.Sprintf("   Time saved: %s\n", sf.timeFormatter.FormatDuration(totalMetrics.TotalSaved))
	stats += fmt.Sprintf("   Avg words/session: %d\n", totalMetrics.AvgWordsPerSession)
	stats += fmt.Sprintf("   Avg saved/session: %s", sf.timeFormatter.FormatDurationShort(totalMetrics.AvgSavedPerSession))

	return stats
}

func (sf *StatsFormatter) FormatWeeklyStats(weeklyMetrics []*DailyMetrics) string {
	if len(weeklyMetrics) == 0 {
		return "📅 No weekly data available yet."
	}

	totalWords := 0
	totalSaved := time.Duration(0)
	totalSessions := 0
	activeDays := 0

	for _, day := range weeklyMetrics {
		if day.SessionCount > 0 {
			activeDays++
			totalWords += day.TotalWords
			totalSaved += day.TotalSaved
			totalSessions += day.SessionCount
		}
	}

	if activeDays == 0 {
		return "📅 No activity this week yet."
	}

	stats := "📅 This Week:\n"
	stats += fmt.Sprintf("   Active days: %d/7\n", activeDays)
	stats += fmt.Sprintf("   Total words: %d\n", totalWords)
	stats += fmt.Sprintf("   Total sessions: %d\n", totalSessions)
	stats += fmt.Sprintf("   Time saved: %s", sf.timeFormatter.FormatDuration(totalSaved))

	return stats
}
