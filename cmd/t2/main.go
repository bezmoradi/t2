package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/bezmoradi/t2/internal/app"
	"github.com/bezmoradi/t2/internal/config"
	"github.com/bezmoradi/t2/internal/version"
)

func main() {
	var (
		resetKey    = flag.Bool("reset-key", false, "Reset/reconfigure AssemblyAI API key")
		showConfig  = flag.Bool("show-config", false, "Show current configuration location")
		showVersion = flag.Bool("version", false, "Show current version")
		verbose     = flag.Bool("v", false, "Print per-recording diagnostics (timings, model, billed seconds)")
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

	instanceLock, err := app.LockInstance()
	if err != nil {
		log.Fatalf("%v", err)
	}
	defer instanceLock.Close()

	if *resetKey {
		handleResetKey()
	}

	go notifyIfOutdated()

	daemon := app.NewDaemon(*verbose)
	if err := daemon.Initialize(); err != nil {
		log.Fatalf("Failed to initialize daemon: %v", err)
	}

	if err := daemon.Run(); err != nil {
		log.Fatalf("Daemon error: %v", err)
	}
}

func notifyIfOutdated() {
	upToDate, newVersion := version.CheckVersion()
	if upToDate {
		return
	}

	fmt.Printf("\n💡 T2 %s is available (you have %s). %s\n", newVersion, version.VERSION, version.UpdateMessage)
	fmt.Printf("   go install github.com/bezmoradi/t2/cmd/t2@%s\n\n", newVersion)
}

func handleShowConfig() {
	configPath, err := config.GetConfigPath()
	if err != nil {
		fmt.Printf("❌ Error getting config path: %v\n", err)
		os.Exit(1)
	}

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		fmt.Println("📝 Config file does not exist yet")
		return
	}

	fmt.Printf("📁 Config file location: %s\n", configPath)
	fmt.Println()

	content, err := os.ReadFile(configPath)
	if err != nil {
		fmt.Printf("❌ Error reading config file: %v\n", err)
		return
	}

	var cfg config.Config
	if err := json.Unmarshal(content, &cfg); err != nil {
		fmt.Println("📋 Config file is present but could not be parsed")
		return
	}

	fmt.Printf("🔑 AssemblyAI key: %s\n", maskKey(cfg.AssemblyAIKey))
	if os.Getenv("ASSEMBLYAI_API_KEY") != "" {
		fmt.Println("⚠️  ASSEMBLYAI_API_KEY is set in your environment and is used instead of this one.")
	}
}

func maskKey(key string) string {
	if key == "" {
		return "(not set)"
	}
	if len(key) <= 8 {
		return "********"
	}
	return key[:4] + "…" + key[len(key)-4:]
}

func handleResetKey() {
	configPath, _ := config.GetConfigPath()
	if err := os.Remove(configPath); err != nil && !os.IsNotExist(err) {
		fmt.Printf("⚠️  Warning: Failed to remove existing config: %v\n", err)
	}

	fmt.Println("🔄 Saved API key removed.")

	if os.Getenv("ASSEMBLYAI_API_KEY") != "" {
		fmt.Println("⚠️  ASSEMBLYAI_API_KEY is still set in your environment and will be used instead.")
	} else {
		fmt.Println("   You'll be prompted for a new one.")
	}
}

func handleShowVersion() {
	fmt.Printf("T2 (Talk to Text) %s\n", version.VERSION)
}
