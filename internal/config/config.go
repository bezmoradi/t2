package config

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"golang.org/x/term"
)

const (
	configFileName = "config.json"
	configDirName  = "t2"
)

// Config represents the application configuration
type Config struct {
	AssemblyAIKey string `json:"assemblyai_key"`
}

// stdin is shared by every prompt, so a line buffered while reading one
// answer is still there for the next.
var stdin = bufio.NewReader(os.Stdin)

// GetConfigDir returns t2's config directory, ~/.config/t2.
func GetConfigDir() (string, error) {
	usr, err := user.Current()
	if err != nil {
		return "", err
	}
	return filepath.Join(usr.HomeDir, ".config", configDirName), nil
}

// EnsureConfigDir creates the config directory if needed and makes it
// private to the user. It holds the API key and recordings of the user's
// voice, so other accounts on the Mac must not be able to list or read it.
func EnsureConfigDir() (string, error) {
	dir, err := GetConfigDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// MkdirAll leaves an existing directory's mode alone, and older versions
	// created this one 0755.
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// GetConfigPath returns the full path to the config file.
func GetConfigPath() (string, error) {
	configDir, err := GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, configFileName), nil
}

// LoadConfig loads configuration from file
func LoadConfig() (*Config, error) {
	configPath, err := GetConfigPath()
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

// SaveConfig saves configuration to file, readable by the user only.
func SaveConfig(config *Config) error {
	if _, err := EnsureConfigDir(); err != nil {
		return err
	}

	configPath, err := GetConfigPath()
	if err != nil {
		return err
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}

	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		return err
	}
	// WriteFile only applies the mode when it creates the file; a key file
	// that already existed with a looser mode would keep it.
	return os.Chmod(configPath, 0o600)
}

// promptForAPIKey prompts user to enter their AssemblyAI API key
func promptForAPIKey() (string, error) {
	fmt.Println("🔑 AssemblyAI API key not found.")
	fmt.Println("📋 To get your free API key:")
	fmt.Println("   1. Visit: https://www.assemblyai.com/")
	fmt.Println("   2. Sign up and get your API key from the dashboard")
	fmt.Println("   3. You get 5 hours of free transcription monthly")
	fmt.Println()
	fmt.Print("🔐 Please enter your AssemblyAI API key (input is hidden): ")

	apiKey, err := readSecret()
	if err != nil {
		return "", fmt.Errorf("failed to read input: %w", err)
	}

	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return "", fmt.Errorf("API key cannot be empty")
	}

	return apiKey, nil
}

// readSecret reads a line without echoing it when stdin is a terminal, so
// the key does not end up on screen or in terminal scrollback.
func readSecret() (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		b, err := term.ReadPassword(fd)
		fmt.Println()
		return string(b), err
	}
	return readLine()
}

// readLine reads one line from stdin. Input that ends without a newline still
// counts; nothing at all is io.EOF.
func readLine() (string, error) {
	line, err := stdin.ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// validateAPIKey performs a basic validation of the API key format
func validateAPIKey(apiKey string) bool {
	// AssemblyAI keys are typically 32 characters long
	return len(apiKey) >= 30 && len(apiKey) <= 50
}

// GetAPIKey returns the AssemblyAI API key from, in order: the
// ASSEMBLYAI_API_KEY environment variable, ~/.config/t2/config.json, or an
// interactive prompt whose answer is saved to that file.
//
// No .env file is read. One used to be read from the current working
// directory, which meant running t2 inside another project silently billed
// that project's AssemblyAI account; after that it was read from the config
// directory, where nothing documented it and --reset-key did not remove it.
func GetAPIKey() (string, error) {
	if apiKey := os.Getenv("ASSEMBLYAI_API_KEY"); apiKey != "" {
		return apiKey, nil
	}

	config, err := LoadConfig()
	if err == nil && config.AssemblyAIKey != "" {
		return config.AssemblyAIKey, nil
	}

	apiKey, err := promptForAPIKey()
	if err != nil {
		return "", err
	}

	if !validateAPIKey(apiKey) {
		fmt.Println("⚠️  Warning: API key format seems unusual (expected 30-50 characters)")
		fmt.Print("🤔 Continue anyway? (y/n): ")
		// No answer - end of input - is a no, not a yes.
		response, err := readLine()
		response = strings.ToLower(strings.TrimSpace(response))
		if err != nil || (response != "y" && response != "yes") {
			return "", fmt.Errorf("API key validation cancelled")
		}
	}

	if err := SaveConfig(&Config{AssemblyAIKey: apiKey}); err != nil {
		fmt.Printf("⚠️  Warning: Failed to save API key: %v\n", err)
		fmt.Println("💡 You'll need to enter it again next time")
	} else {
		configPath, _ := GetConfigPath()
		fmt.Printf("✅ API key saved securely to %s\n", configPath)
	}

	return apiKey, nil
}
