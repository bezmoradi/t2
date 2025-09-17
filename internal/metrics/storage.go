package metrics

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type Storage struct {
	baseDir string
}

const (
	userSettingsFile = "settings.json"
	dailyMetricsDir  = "daily"
)

func NewStorage(baseDir string) (*Storage, error) {
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create metrics directory: %v", err)
	}

	dailyDir := filepath.Join(baseDir, dailyMetricsDir)
	if err := os.MkdirAll(dailyDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create daily metrics directory: %v", err)
	}

	return &Storage{
		baseDir: baseDir,
	}, nil
}

func (s *Storage) SaveSession(session *SessionMetrics) error {
	date := session.Timestamp.Format("2006-01-02")

	// Load or create daily metrics
	dailyMetrics, err := s.GetDailyMetrics(date)
	if err != nil {
		dailyMetrics = &DailyMetrics{
			Date:     date,
			Sessions: []SessionMetrics{},
		}
	}

	// Add session to daily metrics
	dailyMetrics.Sessions = append(dailyMetrics.Sessions, *session)

	// Update daily totals
	dailyMetrics.TotalWords += session.WordCount
	dailyMetrics.TotalSaved += session.TimeSaved
	dailyMetrics.SessionCount = len(dailyMetrics.Sessions)

	return s.saveDailyMetrics(dailyMetrics)
}

func (s *Storage) GetDailyMetrics(date string) (*DailyMetrics, error) {
	filePath := filepath.Join(s.baseDir, dailyMetricsDir, fmt.Sprintf("%s.json", date))

	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		return &DailyMetrics{
			Date:     date,
			Sessions: []SessionMetrics{},
		}, nil
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	var dailyMetrics DailyMetrics
	if err := json.Unmarshal(data, &dailyMetrics); err != nil {
		return nil, err
	}

	return &dailyMetrics, nil
}

func (s *Storage) saveDailyMetrics(metrics *DailyMetrics) error {
	filePath := filepath.Join(s.baseDir, dailyMetricsDir, fmt.Sprintf("%s.json", metrics.Date))

	data, err := json.MarshalIndent(metrics, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(filePath, data, 0644)
}

func (s *Storage) GetTotalMetrics() (*TotalMetrics, error) {
	dailyDir := filepath.Join(s.baseDir, dailyMetricsDir)

	files, err := os.ReadDir(dailyDir)
	if err != nil {
		return &TotalMetrics{}, nil // Return empty metrics if directory doesn't exist
	}

	totalMetrics := &TotalMetrics{}

	for _, file := range files {
		if !file.IsDir() && filepath.Ext(file.Name()) == ".json" {
			filePath := filepath.Join(dailyDir, file.Name())

			data, err := os.ReadFile(filePath)
			if err != nil {
				continue // Skip problematic files
			}

			var dailyMetrics DailyMetrics
			if err := json.Unmarshal(data, &dailyMetrics); err != nil {
				continue // Skip problematic files
			}

			totalMetrics.TotalWords += dailyMetrics.TotalWords
			totalMetrics.TotalSessions += dailyMetrics.SessionCount
			totalMetrics.TotalSaved += dailyMetrics.TotalSaved
		}
	}

	// Calculate averages
	if totalMetrics.TotalSessions > 0 {
		totalMetrics.AvgWordsPerSession = totalMetrics.TotalWords / totalMetrics.TotalSessions
		totalMetrics.AvgSavedPerSession = totalMetrics.TotalSaved / time.Duration(totalMetrics.TotalSessions)
	}

	return totalMetrics, nil
}

func (s *Storage) GetWeeklyMetrics(startDate time.Time) ([]*DailyMetrics, error) {
	var weeklyMetrics []*DailyMetrics

	for i := 0; i < 7; i++ {
		date := startDate.AddDate(0, 0, i).Format("2006-01-02")
		dailyMetrics, err := s.GetDailyMetrics(date)
		if err != nil {
			continue // Skip problematic days
		}
		weeklyMetrics = append(weeklyMetrics, dailyMetrics)
	}

	return weeklyMetrics, nil
}

func (s *Storage) GetRecentDays(days int) ([]*DailyMetrics, error) {
	var recentMetrics []*DailyMetrics

	for i := days - 1; i >= 0; i-- {
		date := time.Now().AddDate(0, 0, -i).Format("2006-01-02")
		dailyMetrics, err := s.GetDailyMetrics(date)
		if err != nil {
			continue // Skip problematic days
		}
		recentMetrics = append(recentMetrics, dailyMetrics)
	}

	return recentMetrics, nil
}

func (s *Storage) SaveUserSettings(settings *UserSettings) error {
	filePath := filepath.Join(s.baseDir, userSettingsFile)

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(filePath, data, 0644)
}

func (s *Storage) LoadUserSettings() (*UserSettings, error) {
	filePath := filepath.Join(s.baseDir, userSettingsFile)

	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		return nil, fmt.Errorf("user settings not found")
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	var settings UserSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, err
	}

	return &settings, nil
}

func (s *Storage) ClearAllMetrics() error {
	dailyDir := filepath.Join(s.baseDir, dailyMetricsDir)

	files, err := os.ReadDir(dailyDir)
	if err != nil {
		return nil // Directory doesn't exist, nothing to clear
	}

	for _, file := range files {
		if !file.IsDir() && filepath.Ext(file.Name()) == ".json" {
			filePath := filepath.Join(dailyDir, file.Name())
			if err := os.Remove(filePath); err != nil {
				return fmt.Errorf("failed to remove %s: %v", file.Name(), err)
			}
		}
	}

	return nil
}

func (s *Storage) GetAllDailyMetrics() ([]*DailyMetrics, error) {
	dailyDir := filepath.Join(s.baseDir, dailyMetricsDir)

	files, err := os.ReadDir(dailyDir)
	if err != nil {
		return []*DailyMetrics{}, nil
	}

	var allMetrics []*DailyMetrics
	var fileNames []string

	for _, file := range files {
		if !file.IsDir() && filepath.Ext(file.Name()) == ".json" {
			fileNames = append(fileNames, file.Name())
		}
	}

	// Sort file names to get chronological order
	sort.Strings(fileNames)

	for _, fileName := range fileNames {
		filePath := filepath.Join(dailyDir, fileName)

		data, err := os.ReadFile(filePath)
		if err != nil {
			continue
		}

		var dailyMetrics DailyMetrics
		if err := json.Unmarshal(data, &dailyMetrics); err != nil {
			continue
		}

		allMetrics = append(allMetrics, &dailyMetrics)
	}

	return allMetrics, nil
}
