# T2 (aka Talk to Text)

T2 is a CLI application written in Go that transforms your voice into text using AI transcription. Hold Ctrl + Shift, speak, let go, and the text is pasted into the app you're using. It runs on macOS and Linux.

## Table of Contents

-   [Installation](#installation)
-   [Prerequisites](#prerequisites)
-   [Setting up AssemblyAI API Key](#setting-up-assemblyai-api-key)
-   [Usage](#usage)
-   [Application Commands](#application-commands)
-   [Building from Source](#building-from-source)
-   [Supported Platforms](#supported-platforms)

## Installation

Install the latest release with one command, on macOS or Linux, on Intel/AMD (amd64) or ARM (arm64):

```sh
$ curl -fsSL https://github.com/bezmoradi/t2/releases/latest/download/install.sh | sh
```

It needs `curl`, `tar`, and `sha256sum` or `shasum`, and no Go, no Xcode Command Line Tools and no sudo. It downloads the archive for your platform, checks it against the SHA-256 the script carries and the archive's own `SHA256SUMS`, and installs `t2` into `~/.local/bin`, or the folder in `T2_INSTALL_DIR`:

```sh
$ curl -fsSL https://github.com/bezmoradi/t2/releases/latest/download/install.sh | T2_INSTALL_DIR="$HOME/bin" sh
```

If anything fails, it stops with one line and leaves the folder as it was. On Linux it then warns about each [run-time tool](#linux) that isn't installed, with the package to install. If the folder isn't on your `PATH`, it prints the line to add to your shell's profile. Run the same command again to update; when a newer version is out, t2 prints the command at startup.

To check the script before you run it, download it and verify that this repository's release workflow built it (with a signed-in [`gh`](https://cli.github.com)):

```sh
$ curl -fsSLO https://github.com/bezmoradi/t2/releases/latest/download/install.sh
$ gh attestation verify install.sh -R bezmoradi/t2 --signer-workflow bezmoradi/t2/.github/workflows/release.yml
$ sh install.sh
```

Then run `t2`. That's it! The app will guide you through the API key setup on first run.

### With Go

You can also build and install t2 with Go:

```sh
$ go install github.com/bezmoradi/t2/cmd/t2@latest
```

That needs Go, and on macOS also Apple's Command Line Tools: t2 calls macOS audio and keyboard frameworks through cgo, which needs a C compiler. The Linux build needs no C compiler.

```sh
$ brew install go
$ xcode-select --install
```

On macOS without the Command Line Tools the install still succeeds, but t2 stops at startup with a message saying so.

`go install` puts `t2` in Go's `bin` folder, so make sure Go's binaries are available in your `PATH`. Add the following line to your shell configuration file:

```sh
export PATH=$PATH:$HOME/go/bin
```

If you use `zsh`, add it to your `~/.zshrc` file, then apply the changes:

```sh
$ echo 'export PATH=$PATH:$HOME/go/bin' >> ~/.zshrc
$ source ~/.zshrc
```

If you use `bash`, add it to your `~/.bash_profile` or `~/.bashrc` file, then apply the changes:

```sh
$ echo 'export PATH=$PATH:$HOME/go/bin' >> ~/.bash_profile
$ source ~/.bash_profile
```

If you’re not sure which shell you’re using, run:

```sh
$ echo $SHELL
```

That will tell you whether you’re in `zsh`, `bash`, or another shell. Update the correct configuration file accordingly.

## Prerequisites

### macOS

The release builds run on macOS 13 Ventura or later and need nothing else installed.

### Linux

At run time t2 calls these tools, so install the ones for your session (`echo $XDG_SESSION_TYPE` prints `x11` or `wayland`):

| What for | Tool | Debian / Ubuntu | Fedora | Arch |
|---|---|---|---|---|
| Recording and beeps (PulseAudio, or PipeWire with `pipewire-pulse`) | `parecord`, `pacat` | `pulseaudio-utils` | `pulseaudio-utils` | `libpulse` |
| Clipboard on X11 | `xclip` (or `xsel`) | `xclip` | `xclip` | `xclip` |
| Pasting on X11 | `xdotool` | `xdotool` | `xdotool` | `xdotool` |
| Clipboard on Wayland | `wl-copy` | `wl-clipboard` | `wl-clipboard` | `wl-clipboard` |
| Typing on Wayland (labwc, Sway, Hyprland, COSMIC, niri and other compositors with the virtual-keyboard protocol) | `wtype` | `wtype` | `wtype` | `wtype` |

For example, on Ubuntu with X11:

```sh
$ sudo apt install pulseaudio-utils xclip xdotool
```

t2 prints a warning at startup if a clipboard or paste tool is missing, or if the sound server has no microphone.

**On Wayland, t2 needs to be in the `input` group to see Ctrl + Shift.** A Wayland desktop doesn't let one program see keys typed into another, so t2 reads the key state from the kernel's input devices (`/dev/input`) instead:

```sh
$ sudo usermod -aG input $USER
```

Then log out and back in. Be aware that this lets every program you run read, and also send, every keystroke, not only t2. t2 itself only checks which keys are down, about ten times a second, and never records keystrokes. On X11 none of this is needed: t2 asks the X server, which any program can do. If no keyboard is connected when t2 starts, it waits and uses one as soon as it's plugged in. It only sees keyboards attached to the machine: keys typed over SSH, or through screen sharing, usually don't reach `/dev/input`.

## Setting up AssemblyAI API Key

When you run the app for the first time, you'll see:

```text
🔑 AssemblyAI API key not found.
📋 To get your free API key:
   1. Visit: https://www.assemblyai.com
   2. Sign up and get your API key from the dashboard
   3. You get 5 hours of free transcription monthly

🔐 Please enter your AssemblyAI API key (input is hidden):
```

The key is saved to `~/.config/t2/config.json`, readable by you only.

## Usage

1. **Start**: Run `t2`
2. **Record**: Hold "Ctrl + Shift" key to start recording
3. **Speak**: Talk into your microphone
4. **Stop**: Release the keys to stop recording
5. **Wait**: AI processes your audio (shows progress)
6. **Auto-paste**: Text is automatically pasted to your active application

On Linux, pasting depends on the desktop:

| Session | What happens |
|---|---|
| X11 | Pasted with Ctrl + V |
| Wayland on labwc, Sway, Hyprland, COSMIC, niri and other compositors with the virtual-keyboard protocol, with `wtype` installed | Typed into the app, as if from a keyboard, and also copied to the clipboard |
| Wayland on GNOME or KDE Plasma | Copied to the clipboard; press Ctrl + V yourself. These desktops don't accept keystrokes sent by other programs |

On Wayland, t2 types the text instead of pressing Ctrl + V, because GTK3 apps, such as Mousepad, ignore shortcuts sent by `wtype`. Typing takes a moment: about 4 seconds per 1,000 characters on a Raspberry Pi 5. Wait for it to finish before you type, or your keys land in the middle of it.

On X11, most terminals paste with Ctrl + Shift + V instead, so in a terminal the Ctrl + V that t2 sends does nothing useful. Press Ctrl + Shift + V; the text is on the clipboard.

On GNOME under Wayland, `wl-copy` has to briefly open a window to reach the clipboard. If that doesn't work, t2 gives up after 3 seconds, says so, and prints the text.

Good to know:

-   You can start the next dictation straight away. It records from the moment you press, while the previous one is still being transcribed. Results are pasted in the order you spoke them, each once you've let go of the keys.
-   Presses shorter than 0.4 seconds are ignored, and never connect to AssemblyAI.
-   Pressing any other key while holding Ctrl + Shift (a shortcut such as Ctrl + Shift + Tab) stops the recording at once, without transcribing it. If you had been talking for 3 seconds or more, the audio is saved (see below).
-   If a recording can't be transcribed in full (no network, a dropped connection, a cancelled dictation), the audio is saved to `~/.config/t2/last-recording.wav`, readable by you only. There is one such file: the next failure replaces it, and the next successful transcription deletes it.
-   Only one t2 can run at a time; a second one exits with a message.

## Application Commands

```sh
# Show version
./t2 --version

# Show config file location
./t2 --show-config

# Reset API key (removes the saved key; you're prompted for a new one)
./t2 --reset-key
```

## Building from Source

Clone the repository by running the following command:

```sh
$ git clone git@github.com:bezmoradi/t2.git
```

Then CD into the `t2` folder and build the app (the same command on macOS and Linux; it needs what [With Go](#with-go) lists):

```sh
$ go build -o t2 ./cmd/t2/
```

Finally, run the app by the following command:

```sh
$ ./t2
```

To build a release locally into `dist`, from a clean checkout on macOS (the darwin builds need cgo; linux builds without it anywhere), run `scripts/release.sh <version>`, with the version `internal/version/version.go` sets, without its `v`. It writes the four archives, `install.sh` and `SHA256SUMS`. A `v*` tag on main that matches that version makes `.github/workflows/release.yml` build, check, sign and draft the release; the owner publishes it.

### API Key Priority System

Your API key is read in this order:

1. The `ASSEMBLYAI_API_KEY` environment variable
2. User config file at `~/.config/t2/config.json` (recommended)
3. Interactive prompt (first-time setup), whose answer is saved to that file

No `.env` file is read.

## Supported Platforms

-   ✅ **macOS** (fully supported)
-   ✅ **Linux**
-   ❌ **Windows** (planned)
