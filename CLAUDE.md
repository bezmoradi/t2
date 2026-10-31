# CLAUDE.md

Guidance for Claude Code in this repository. User-facing setup and behaviour are in README.md; this file covers what the code doesn't make obvious.

T2 (Talk to Text) is a Go CLI daemon: hold Ctrl+Shift to record, release to have AssemblyAI's streaming API transcribe it, and the text is pasted into the focused app. It runs on macOS (cgo) and Linux (no cgo).

## Commands

```bash
go build -o t2 ./cmd/t2          # build
go run -race ./cmd/t2 -v         # run from source; -v prints timings, chunks, billed seconds
go install github.com/bezmoradi/t2/cmd/t2@main

# Checks to run before any commit: every platform must vet and build
go vet ./... && GOOS=linux CGO_ENABLED=0 go vet ./... && GOOS=windows go vet ./...
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build ./... && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...
```

Running the real binary creates `~/.config/t2` (mode 0700) and its lock file, and exits if another t2 holds the lock. For experiments, use `HOME=$(mktemp -d) ASSEMBLYAI_API_KEY=dummy ./t2`.

## Testing

The project has no automated tests and relies on human end-to-end testing; don't add tests. Verify a change by vetting and building every platform, then by running the real binary.

Linux can be run end to end in Docker (`golang:1.27-bookworm`):

- **Microphone:** a PulseAudio null sink, with its monitor as the default source and speech played into it with `paplay --latency-msec=20`. A null sink runs in 2 s blocks unless some stream asks for low latency, which delays the first audio by up to 2 s. Keep `pacat --latency-msec=20 -d <sink> < /dev/zero` running. PulseAudio run under `su` needs `XDG_RUNTIME_DIR` set, as a login session would.
- **X11 keys:** Xvfb plus `xdotool keydown`/`keyup`. t2 polls every 100 ms, and `xdotool key` taps for about 12 ms, so it's usually missed. Hold other keys for 150 ms, as a person does.
- **The /dev/input path:** a uinput virtual keyboard in a `--privileged` container with `-v /dev/input:/dev/input`, and `WAYLAND_DISPLAY` set so t2 takes that path. The Debian image has no `input` group; create it.
- **The API key:** use a dummy key. The dial then fails authentication and nothing is billed.

## Constraints

- **AssemblyAI bills the wall-clock time a session is open, idle included.** Never open a session outside a recording. The dial waits until a hold reaches 400 ms (`quickPressThreshold`), so taps and quick shortcuts never connect. Capture starts on the press and is buffered meanwhile. Every skip path closes a session that was opened. `speechModel` is pinned because the server default costs three times as much.
- **The API key** comes from `ASSEMBLYAI_API_KEY`, then `~/.config/t2/config.json` (0600, in a 0700 directory), then a hidden prompt that saves it. No `.env` file is read. Never print or log the key.
- **Text from the server** (transcripts, error messages, close reasons) goes through `transcription.Sanitize` (or the daemon's `errText`) before it's printed or pasted, because it can contain terminal control characters. Server messages are capped at 1 MiB, and the update check's response body at 64 KiB.
- **The paste keystroke is sent once and never retried.** osascript and the Linux tools can report a failure after the keystroke was delivered, and a second paste corrupts what the user is typing. The text stays on the clipboard either way.
- **No cgo on Linux.** `go install` there must work with no C compiler and no `-dev` packages. That's why the X11 poller uses `github.com/jezek/xgb` instead of Xlib, and why capture and beeps run `parecord` and `pacat`.
- **GitHub text:** `.github/workflows/claude.yml` reacts to mentions of the bot. Put handles in backticks in issues, PRs and comments.
- **Toolchain:** go.mod requires go 1.27.3, for the patched crypto/tls and crypto/x509. It pins `godebug x509sslcertoverrideplatform=0` so certificate checks stay on Apple's verifier.

## Platform split

Files are split by build constraint. `*_darwin.go` (cgo where needed) and `*_linux.go` hold the real backends. `*_other.go` are stubs that compile everywhere else, and on macOS without a C compiler, and fail at startup with one sentence instead of failing to build. Shared code must not name a platform's tools or settings; ask the package instead (`clipboard.PasteKeys`, `audio.SoundSettings()`, `audio.Backend()`).

| Package | macOS | Linux |
|---|---|---|
| `internal/audio` | AudioQueue (`aq_darwin.c`), afplay beeps | One `parecord --raw` process per recording (PulseAudio, or PipeWire through pipewire-pulse); generated tones through `pacat` |
| `internal/hotkeys` | CoreGraphics key state (`keys_darwin.go`) | The X server's key map through xgb when `DISPLAY` is set and `WAYLAND_DISPLAY` isn't (`x11_linux.go`); otherwise EVIOCGKEY on every `/dev/input` keyboard (`evdev_linux.go`) |
| `internal/clipboard` | pbcopy + osascript Cmd+V | xclip/xsel + xdotool on X11; wl-copy + wtype on Wayland |

## Architecture

**Daemon (`internal/app/daemon.go`).**
- A press starts capture and the start beep.
- A release only stops capture. Everything after it runs on a per-hold `take` in an ordered background queue: the dial result, flushing the audio, `Finish`, and the paste. So the next hold can record at once, and results paste in the order they were spoken, each once the hotkey is up (`waitKeysUp`).
- Skip paths, checked first: a cancelled hold (another key pressed), a quick press under 400 ms, no audio at all, and silence below RMS 150. None of them transcribes. A hold cancelled after 3 s or more is saved.
- Failures save `~/.config/t2/last-recording.wav` (0600). There is one slot, and the next clean success deletes it.
- One instance runs at a time, enforced by an flock on `~/.config/t2/t2.lock`.

**Sink (`internal/app/sink.go`).** The capture goroutine only appends audio. A per-recording sender goroutine streams it to the session. That way a stalled WebSocket write can neither overflow capture nor look like a stuck stop. Frames are 60–800 ms, with the last one padded: the API closes the session (3007) on frames outside 50–1000 ms.

**Audio (`internal/audio`).**
- `Stream` is the backend interface; `OpenStream` opens the platform's backend for each recording, on the current default microphone.
- The capture loop never blocks inside the audio layer: it polls `Available` and reads only whole chunks.
- `Recorder.Stop` bounds its waits. If a stream won't close, `Outcome.Wedged` is set, and the daemon finishes the recording, waits 10 s, then re-execs itself (`restartProcess`). That's limited to 3 times an hour, counted in `T2_RESTART_TIMES`, and `--reset-key` is dropped from the new argv.
- This watchdog exists because PortAudio's CoreAudio backend deadlocked in `Pa_StopStream` and left t2 deaf until restarted. AudioQueue replaced PortAudio for that reason. On Linux, Close kills `parecord`, so it can't hang, and `Pdeathsig` stops the microphone staying open if t2 dies.

**Hotkeys (`internal/hotkeys`).**
- One poller (`simple.go`, every 100 ms) runs over two per-platform probes, `ctrlShiftDown` and `otherKeyDown`, so a hold behaves the same everywhere:
  - Ctrl+Shift alone starts it;
  - Alt or Super (Cmd or Option on macOS) with it rejects it;
  - any other key cancels it at once;
  - a hold is capped at `MaxHold` (5 minutes).
- After a cancel or the cap, the keys are ignored until they physically come up, so a stuck modifier can't keep the microphone open.
- The `/dev/input` path starts even with no keyboard, unless a device was refused for lack of permission (then it stops with the `input` group message). The 3 s rescan picks up a keyboard plugged in later. Refused devices are retried on every rescan, because a node is briefly root's before udev hands it to the `input` group.
- On Wayland, no program can see keys typed into another program's window, X11 programs under XWayland included. That's why Wayland uses `/dev/input`, which needs the `input` group. The GlobalShortcuts portal was rejected for two reasons: a trigger can't be modifier-only, and it can't report the cancel.

**Clipboard (`internal/clipboard`).**
- GNOME and KDE on Wayland accept no synthetic keystrokes. There, `PasteTextSafely` returns `ErrCopiedOnly`, which the daemon reports as "Copied", not as a failure.
- Don't capture the output of the Linux clipboard tools. They fork a child that keeps serving the text and inherits any output pipe, so collecting stdout or stderr waits until the next copy.
- Every tool runs with a 3 s timeout, because wl-copy can hang on GNOME.

**Transcription (`internal/transcription`).**
- Turn detection tolerates thinking pauses (`max_turn_silence` 5000 ms, `min_turn_silence` 700 ms, `end_of_turn_confidence_threshold` 0.85), so a dictation isn't split into one capitalised sentence per pause. The key release ends the last turn anyway.
- These can be tuned with `T2_MAX_TURN_SILENCE_MS`, `T2_MIN_TURN_SILENCE_MS` and `T2_END_OF_TURN_CONFIDENCE`. `T2_TURN_DEFAULTS=1` uses the server's defaults.
- The Begin message doesn't carry the speech model, so the model shown by `-v` is blank.

**Startup checks.** `Daemon.Run` collects `audio.Limitation()` and `clipboard.Limitation()` before starting the hotkey, and prints them even when that start fails, so a machine missing several things learns all of them in one run. On Linux, `audio.Limitation` asks `pactl` for the sources and warns when there is no microphone (only `.monitor` sources), when the default source is a monitor, or when the sound server can't be reached. `parecord` records a monitor without complaint, so without this check a machine with no microphone recorded what it was playing.

**Diagnostics.** `T2_FRAMES_PER_BUFFER=32 t2` runs with 32-frame capture buffers (default 1024, allowed 32–8192). That was the setting that made the PortAudio deadlock reproducible.

## Platform status

- **macOS:** supported.
- **Linux:** X11, and Wayland with the `input` group. Tested only in Docker: Xvfb, a uinput keyboard, a PulseAudio null sink, and one live dictation pasted on X11. Not yet run on a physical desktop.
- **Windows:** stubs only.
