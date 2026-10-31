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

	privateDirMode  = 0o700
	privateFileMode = 0o600

	minPlausibleKeyLength = 30
	maxPlausibleKeyLength = 50
)

type Config struct {
	AssemblyAIKey string `json:"assemblyai_key"`
}

var stdinSharedByAllPrompts = bufio.NewReader(os.Stdin)

func GetConfigDir() (string, error) {
	usr, err := user.Current()
	if err != nil {
		return "", err
	}
	return filepath.Join(usr.HomeDir, ".config", configDirName), nil
}

func EnsureConfigDir() (string, error) {
	dir, err := GetConfigDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, privateDirMode); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, privateDirMode); err != nil {
		return "", err
	}
	return dir, nil
}

func GetConfigPath() (string, error) {
	configDir, err := GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, configFileName), nil
}

func LoadConfig() (*Config, error) {
	configPath, err := GetConfigPath()
	if err != nil {
		return nil, err
	}

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return &Config{}, nil
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

	if err := os.WriteFile(configPath, data, privateFileMode); err != nil {
		return err
	}
	return os.Chmod(configPath, privateFileMode)
}

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

func readSecret() (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		b, err := term.ReadPassword(fd)
		fmt.Println()
		return string(b), err
	}
	return readLine()
}

func readLine() (string, error) {
	line, err := stdinSharedByAllPrompts.ReadString('\n')
	lastLineWithoutNewline := errors.Is(err, io.EOF) && line != ""
	if err != nil && !lastLineWithoutNewline {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func keyLengthPlausible(apiKey string) bool {
	return len(apiKey) >= minPlausibleKeyLength && len(apiKey) <= maxPlausibleKeyLength
}

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

	if !keyLengthPlausible(apiKey) {
		fmt.Println("⚠️  Warning: API key format seems unusual (expected 30-50 characters)")
		fmt.Print("🤔 Continue anyway? (y/n): ")
		response, err := readLine()
		response = strings.ToLower(strings.TrimSpace(response))
		confirmed := err == nil && (response == "y" || response == "yes")
		if !confirmed {
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
