package config

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/joho/godotenv"
)

const (
	configFileName = "config.json"
	configDirName  = "t2"
	metricsSubDir  = "metrics"
)

// Config represents the application configuration
type Config struct {
	AssemblyAIKey string `json:"assemblyai_key"`
	TypingSpeed   int    `json:"typing_speed,omitempty"` // User's typing speed in WPM
}

// getConfigDir returns the user's config directory for T2
func getConfigDir() (string, error) {
	usr, err := user.Current()
	if err != nil {
		return "", err
	}

	configDir := filepath.Join(usr.HomeDir, ".config", configDirName)
	return configDir, nil
}

// getConfigPath returns the full path to the config file
func getConfigPath() (string, error) {
	configDir, err := getConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, configFileName), nil
}

// LoadConfig loads configuration from file
func LoadConfig() (*Config, error) {
	configPath, err := getConfigPath()
	if err != nil {
		return nil, err
	}

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return &Config{}, nil // Return empty config if file doesn't exist
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}

	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	return &config, nil
}

// SaveConfig saves configuration to file
func SaveConfig(config *Config) error {
	configDir, err := getConfigDir()
	if err != nil {
		return err
	}

	// Create config directory if it doesn't exist
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return err
	}

	configPath, err := getConfigPath()
	if err != nil {
		return err
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}

	// Write with user-only permissions for security
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		return err
	}

	return nil
}

// GetConfigPath returns the full path to the config file (exported for CLI commands)
func GetConfigPath() (string, error) {
	return getConfigPath()
}

// promptForAPIKey prompts user to enter their AssemblyAI API key
func promptForAPIKey() (string, error) {
	fmt.Println("🔑 AssemblyAI API key not found.")
	fmt.Println("📋 To get your free API key:")
	fmt.Println("   1. Visit: https://www.assemblyai.com/")
	fmt.Println("   2. Sign up and get your API key from the dashboard")
	fmt.Println("   3. You get 5 hours of free transcription monthly")
	fmt.Println()
	fmt.Print("🔐 Please enter your AssemblyAI API key: ")

	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return "", fmt.Errorf("failed to read input")
	}

	apiKey := strings.TrimSpace(scanner.Text())
	if apiKey == "" {
		return "", fmt.Errorf("API key cannot be empty")
	}

	return apiKey, nil
}

// validateAPIKey performs a basic validation of the API key format
func validateAPIKey(apiKey string) bool {
	// AssemblyAI keys are typically 32 characters long
	return len(apiKey) >= 30 && len(apiKey) <= 50
}

// GetAPIKey retrieves API key using fallback priority system
func GetAPIKey() (string, error) {
	// Priority 1: Environment variable (for power users)
	if apiKey := os.Getenv("ASSEMBLYAI_API_KEY"); apiKey != "" {
		return apiKey, nil
	}

	// Priority 2: .env file (current development setup)
	if err := godotenv.Load(); err == nil {
		if apiKey := os.Getenv("ASSEMBLYAI_API_KEY"); apiKey != "" {
			return apiKey, nil
		}
	}

	// Priority 3: User config file
	config, err := LoadConfig()
	if err == nil && config.AssemblyAIKey != "" {
		return config.AssemblyAIKey, nil
	}

	// Priority 4: Interactive prompt
	apiKey, err := promptForAPIKey()
	if err != nil {
		return "", err
	}

	// Validate API key format
	if !validateAPIKey(apiKey) {
		fmt.Println("⚠️  Warning: API key format seems unusual (expected 30-50 characters)")
		fmt.Print("🤔 Continue anyway? (y/n): ")
		scanner := bufio.NewScanner(os.Stdin)
		if scanner.Scan() {
			response := strings.ToLower(strings.TrimSpace(scanner.Text()))
			if response != "y" && response != "yes" {
				return "", fmt.Errorf("API key validation cancelled")
			}
		}
	}

	// Save the API key for future use
	newConfig := &Config{
		AssemblyAIKey: apiKey,
	}
	if err := SaveConfig(newConfig); err != nil {
		fmt.Printf("⚠️  Warning: Failed to save API key: %v\n", err)
		fmt.Println("💡 You'll need to enter it again next time")
	} else {
		configPath, _ := getConfigPath()
		fmt.Printf("✅ API key saved securely to %s\n", configPath)
	}

	return apiKey, nil
}

// GetMetricsDir returns the metrics directory path
func GetMetricsDir() (string, error) {
	configDir, err := getConfigDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(configDir, metricsSubDir), nil
}
