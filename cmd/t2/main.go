package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"

	"github.com/bezmoradi/t2/internal/app"
	"github.com/bezmoradi/t2/internal/config"
	"github.com/bezmoradi/t2/internal/metrics"
	"github.com/bezmoradi/t2/internal/version"
)

func main() {
	isValid, newVersion := version.CheckVersion()
	if !isValid {
		fmt.Printf(`The newest version of T2 is %v but the installed version on your system is %v.

%v

To get the latest features and likely bugfixes, please install the latest version by running 'go install github.com/bezmoradi/t2/cmd/t2@main'.`+"\n", newVersion, version.VERSION, version.UPDATE_MESSAGE)
		return
	}

	var (
		resetKey       = flag.Bool("reset-key", false, "Reset/reconfigure AssemblyAI API key")
		showConfig     = flag.Bool("show-config", false, "Show current configuration location")
		showVersion    = flag.Bool("version", false, "Show current version")
		showStats      = flag.Bool("stats", false, "Show usage statistics and productivity metrics")
		resetStats     = flag.Bool("reset-stats", false, "Clear all usage statistics")
		setTypingSpeed = flag.String("set-typing-speed", "", "Set your typing speed in words per minute (e.g., --set-typing-speed=65)")
	)
	flag.Parse()

	if *showVersion {
		handleShowVersion()
		return
	}

	if *showConfig {
		handleShowConfig()
		return
	}

	if *showStats {
		handleShowStats()
		return
	}

	if *resetStats {
		handleResetStats()
		return
	}

	if *setTypingSpeed != "" {
		handleSetTypingSpeed(*setTypingSpeed)
		return
	}

	if *resetKey {
		handleResetKey()
	}

	daemon := app.NewDaemon()
	if err := daemon.Initialize(); err != nil {
		log.Fatalf("Failed to initialize daemon: %v", err)
	}

	if err := daemon.Run(); err != nil {
		log.Fatalf("Daemon error: %v", err)
	}
}

func handleShowConfig() {
	configPath, err := config.GetConfigPath()
	if err != nil {
		fmt.Printf("❌ Error getting config path: %v\n", err)
		os.Exit(1)
	}

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		fmt.Println("📝 Config file does not exist yet")
	} else {
		fmt.Printf("📁 Config file location: %s\n", configPath)
		fmt.Println()
		fmt.Println("📋 Config file contents:")

		// Read and display the config file contents
		content, err := os.ReadFile(configPath)
		if err != nil {
			fmt.Printf("❌ Error reading config file: %v\n", err)
			return
		}

		// Pretty print the JSON content
		fmt.Println(string(content))
	}
}

func handleResetKey() {
	configPath, _ := config.GetConfigPath()
	if err := os.Remove(configPath); err != nil && !os.IsNotExist(err) {
		fmt.Printf("⚠️  Warning: Failed to remove existing config: %v\n", err)
	}
	fmt.Println("🔄 API key reset. You'll be prompted for a new one.")
}

func handleShowVersion() {
	fmt.Printf("T2 (Talk to Text) %s\n", version.VERSION)
}

func handleShowStats() {
	metricsDir, err := config.GetMetricsDir()
	if err != nil {
		fmt.Printf("❌ Error getting metrics directory: %v\n", err)
		os.Exit(1)
	}

	metricsManager, err := metrics.NewMetricsManager(metricsDir)
	if err != nil {
		fmt.Printf("❌ Error initializing metrics: %v\n", err)
		os.Exit(1)
	}

	// Get total metrics
	totalMetrics, err := metricsManager.GetTotalMetrics()
	if err != nil {
		fmt.Printf("❌ Error getting total metrics: %v\n", err)
		os.Exit(1)
	}

	// Get recent metrics for context
	recentDays, err := metricsManager.GetRecentDays(7)
	if err != nil {
		fmt.Printf("⚠️  Warning: Failed to get recent metrics: %v\n", err)
	}

	formatter := metrics.NewStatsFormatter()

	// Display total stats
	fmt.Println(formatter.FormatTotalStats(totalMetrics))
	fmt.Println()

	// Display weekly stats if available
	if len(recentDays) > 0 {
		fmt.Println(formatter.FormatWeeklyStats(recentDays))
		fmt.Println()
	}

	// Display typing speed setting
	typingSpeed := metricsManager.GetTypingSpeed()
	fmt.Printf("⌨️  Current typing speed setting: %d WPM\n", typingSpeed)
	fmt.Println("💡 Use --set-typing-speed to update for more accurate time savings")
}

func handleResetStats() {
	metricsDir, err := config.GetMetricsDir()
	if err != nil {
		fmt.Printf("❌ Error getting metrics directory: %v\n", err)
		os.Exit(1)
	}

	metricsManager, err := metrics.NewMetricsManager(metricsDir)
	if err != nil {
		fmt.Printf("❌ Error initializing metrics: %v\n", err)
		os.Exit(1)
	}

	if err := metricsManager.ClearAllMetrics(); err != nil {
		fmt.Printf("❌ Error clearing metrics: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("🗑️  All usage statistics have been cleared")
}

func handleSetTypingSpeed(speedStr string) {
	speed, err := strconv.Atoi(speedStr)
	if err != nil {
		fmt.Printf("❌ Invalid typing speed: %s (must be a number)\n", speedStr)
		os.Exit(1)
	}

	if speed < 10 || speed > 200 {
		fmt.Printf("❌ Typing speed must be between 10 and 200 WPM (got %d)\n", speed)
		os.Exit(1)
	}

	metricsDir, err := config.GetMetricsDir()
	if err != nil {
		fmt.Printf("❌ Error getting metrics directory: %v\n", err)
		os.Exit(1)
	}

	metricsManager, err := metrics.NewMetricsManager(metricsDir)
	if err != nil {
		fmt.Printf("❌ Error initializing metrics: %v\n", err)
		os.Exit(1)
	}

	if err := metricsManager.SetTypingSpeed(speed); err != nil {
		fmt.Printf("❌ Error setting typing speed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("✅ Typing speed updated to %d WPM\n", speed)
	fmt.Println("💡 This will be used to calculate more accurate time savings in future sessions")
}
