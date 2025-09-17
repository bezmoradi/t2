# T2 (aka Talk to Text)

T2 is a CLI application written in Go that transforms your voice into text using AI transcription.

## Table of Contents

-   [Prerequisites](#prerequisites)
-   [Installation](#installation)
-   [Setting up AssemblyAI API Key](#setting-up-assemblyai-api-key)
-   [Usage](#usage)
-   [Application Commands](#application-commands)
-   [Building from Source](#building-from-source)
-   [Supported Platforms](#supported-platforms)

## Prerequisites

Mac users need to run the following command to install the prerequisites:

```sh
$ brew install portaudio pkg-config go
```

## Installation

To install the tool, run the following command inside terminal:

```sh
$ go install github.com/bezmoradi/t2/cmd/t2@main
```

To make sure it's installed correctly in your `$GOPATH`, run the following command:

```sh
$ t2
```

If the installation process goes well, from now on you can run the `t2` command from anywhere on your file system. That's it! The app will guide you through the API key setup on first run.

## Setting up AssemblyAI API Key

When you run the app for the first time, you'll see:

```text
AssemblyAI API key not found.
🔑 AssemblyAI API key not found.
📋 To get your free API key:
   1. Visit: https://www.assemblyai.com
   2. Sign up and get your API key from the dashboard
   3. You get 5 hours of free transcription monthly

🔐 Please enter your AssemblyAI API key:
```

## Usage

1. **Start**: Run `t2`
2. **Record**: Hold "Fn + F1" key to start recording
3. **Speak**: Talk into your microphone
4. **Stop**: Release the keys to stop recording
5. **Wait**: AI processes your audio (shows progress)
6. **Auto-paste**: Text is automatically pasted to your active application

## Application Commands

```sh
# Show version
./t2 --version

# Show config file location
./t2 --show-config

# Reset API key (removes saved config)
./t2 --reset-key

# Show usage statistics and productivity metrics
./t2 --stats

# Clear all usage statistics
./t2 --reset-stats

# Set your typing speed for accurate time savings calculation
./t2 --set-typing-speed=65
```

## Building from Source

Clone the repository by running the following command:

```sh
$ git clone git@github.com:bezmoradi/t2.git
```

Then CD into the `t2` folder and build the app:

```sh
$ go build -o t2 ./cmd/t2/ 
```

Finally, run the app by the following command:

```sh
$ ./t2
```

### API Key Priority System

Your API key is read in this order:

1. `.env` file in the current directory
2. User config file at `~/.config/t2/config.json` (recommended)
3. Interactive prompt (first-time setup)

## Supported Platforms

-   ✅ **macOS** (fully supported)
-   ❌ **Windows** (planned)
-   ❌ **Linux** (planned)
