# T2 — Review Brief for an End-to-End Audit

## What we're asking for

Please review this codebase end to end — correctness, concurrency, architecture, failure
modes, and anything in between — and **write your findings to a new markdown file at the
repository root: `REVIEW.md`.**

Structure `REVIEW.md` however you think best. What would help most:

- Severity-ranked findings with `file:line` anchors and a concrete failure scenario for each
  (specific inputs or interleavings → specific wrong outcome), not just a category label.
- A clear split between **"this is a bug"** and **"this is a design smell I'd change anyway."**
- Explicit callouts where you think a decision recorded below is *wrong*. Several were
  judgment calls made under uncertainty and are documented as such — push back on them.
- Anything you'd want in place before trusting this tool daily.

Don't feel obliged to be diplomatic about the code. It's a personal tool that grew organically
and has never had a second pair of eyes on it.

---

## 1. What this is and why it exists

T2 ("talk to text") is a **single-user, personal macOS CLI dictation tool**. You hold a hotkey,
speak, release, and the transcribed text is pasted into whatever application currently has
focus.

The motivation is cost: the author wanted push-to-talk dictation without paying a monthly
subscription for a commercial Whisper-based product. It calls AssemblyAI's streaming API
directly on the author's own API key.

**Please calibrate to that.** This is explicitly a "quick and dirty tool for myself," not a
product. Recommendations that amount to "add a plugin architecture," "abstract the provider
behind an interface for the four other providers you might add," or "add OpenTelemetry" are
not useful here. Recommendations that amount to "this will silently lose your recording once a
week and here's the interleaving" are extremely useful.

Scale to design for: one user, one machine, a few dozen recordings a day, each 2–60 seconds.

---

## 2. Build, run, test

```bash
# prerequisites (macOS)
brew install portaudio pkg-config go

# build / install
go build -o t2 ./cmd/t2      # local binary
go install ./cmd/t2          # installs to $GOPATH/bin

# test
go test ./... -race

# static analysis (both currently clean)
go vet ./...
staticcheck ./...
go run golang.org/x/tools/cmd/deadcode@latest ./cmd/t2
```

Go 1.26.0, darwin/arm64. Module targets Go 1.24. cgo is required — the hotkey detection is
Objective-C against CoreGraphics/Carbon, so **this does not cross-compile and does not build
on Linux or Windows.**

Runtime requirements that bite: the **terminal application** running `t2` (not the binary)
needs macOS **Microphone** and **Accessibility** permissions. Without Accessibility, the
hotkey never fires *and* the paste keystroke silently fails.

CLI surface is deliberately tiny: `--version`, `--show-config`, `--reset-key`. No flags for
anything else.

API key resolution order (`internal/config/config.go`): `ASSEMBLYAI_API_KEY` env var → `.env`
in the current working directory → `~/.config/t2/config.json` (0600) → interactive prompt,
which then saves.

---

## 3. Architecture and data flow

```
cmd/t2/main.go
  └── version.CheckVersion()          blocking HTTPS GET on every startup (see §7.3)
  └── app.Daemon
        ├── hotkeys.Manager  ──────►  SimpleHotkeyManager (cgo: polls Ctrl+Shift @100ms)
        ├── audio.Recorder   ──────►  PortAudio capture → PCM16 → callback
        ├── transcription.Client ──►  AssemblyAI v3 WebSocket
        ├── transcription.Processor   assembles partial/final transcripts
        └── clipboard.PasteTextSafely pbcopy + AppleScript Cmd+V
```

Packages, all under `internal/`:

| Package | Lines | Role |
|---|---|---|
| `app` | 228 | Daemon: lifecycle, hotkey handlers, orchestration |
| `audio` | 302 + 28 | PortAudio capture, RMS/silence analysis, beeps |
| `transcription` | 304 + 116 | WebSocket client; transcript assembly |
| `hotkeys` | 140 + 32 | cgo modifier polling; thin `Manager` wrapper |
| `config` | 185 | API key resolution and storage |
| `clipboard` | 44 | pbcopy + AppleScript paste |
| `version` | 55 + 4 | Startup version gate |

~1,530 lines of production Go, ~310 lines of tests.

### The recording lifecycle

Read `internal/app/daemon.go` `OnPress`/`OnRelease` alongside this.

1. **Poll** (`hotkeys/simple.go`, own goroutine) samples `CGEventSourceFlagsState` every 100ms.
   On a rising edge it sends `true` into `events`; falling edge sends `false`.
2. **Listen** (own goroutine) reads `events`. On `true` → `OnPress()`, then blocks in
   `waitForRelease()` until `false` → `OnRelease()`.
3. **`OnPress`**: bails if already recording; may `Close()` a degraded connection; reconnects
   if `!IsConnected()`; sleeps 150ms; start beep; `processor.Reset()`; records `pressTime`;
   `recorder.Start()`.
4. **`recorder.Start`** opens a PortAudio stream and spawns `audioStreamLoop`, which reads
   1024 int32 frames at 16kHz (~64ms), converts to PCM16, tracks max RMS, runs silence
   detection, and calls `client.SendAudio` — **synchronously, in the capture loop** — then
   sleeps 10ms.
5. **`OnRelease`**: `recorder.Stop()` (closes stopChan, `streamWg.Wait()`, closes stream);
   stop beep; two skip gates — quick-press (<800ms) and max-RMS below the silence floor;
   `client.Terminate()`; wait up to **1s** for the server's `Termination` message;
   `ConsumeTranscriptWithFallback()`; `processor.Reset()`; paste.
6. **`handleResponses`** (own goroutine, one per connection) reads WebSocket frames, decodes
   into `map[string]any`, and fires callbacks for `Turn` and `Termination`.

### Concurrency map

Goroutines: main (blocks on SIGINT/SIGTERM), poll, listen, audio capture (one per recording),
WebSocket reader (one per connection).

Locks: `Recorder.recordingMutex` guards recording state and the stream pointer;
`Client.wsMutex` guards the connection pointer, health counters, **and all socket writes**;
`Processor.transcriptMutex` guards transcript state.

Cross-goroutine handoffs: `events` (buffered 64, both edges, ordered) drives the handlers;
`stopChan` + `streamWg` shut down capture; `sessionTerminated` (buffered 1) signals server
termination; the transcript callback chain runs **on the WebSocket reader goroutine** and
mutates `Processor` state that `OnRelease` reads on the listen goroutine.

`go test ./... -race` is clean, but the suite does not exercise the real end-to-end path — it
covers the WebSocket lifecycle against a local server, hotkey edge pairing, transcript
assembly, and the version gate. **There is no test that runs a real recording.**

---

## 4. Recent history — please don't re-derive these

The tool was materially broken until very recently. These are fixed and verified; noting them
so you don't spend time rediscovering them, and so you can challenge the *fixes*.

1. **Dead WebSocket passed the liveness check.** `IsConnected()` probed with a ping write, but
   a write to a half-closed TCP socket succeeds. AssemblyAI hangs up after every `Terminate`,
   so the next press reused a spent connection and streamed audio into a socket with no
   reader — a lost recording, roughly every other one. Fixed: the reader goroutine owns
   clearing `wsConn` (with an identity check so a late-exiting reader can't clobber a newer
   connection), `Terminate` sets a `terminated` flag, and `IsConnected` is now a pure state
   check. **Consequence: every press now reconnects.** See §7.1.
2. **Hotkey edges were dropped.** Press and release used two separate size-1 channels with
   `default:` drops. A dropped release edge left the recorder running with no key held. Fixed:
   one ordered buffered channel carrying both edges; the poller blocks rather than drops.
3. **The version gate could brick the CLI.** An unparseable remote body made `extractVersion`
   return `""`, which compared unequal to the local version, so the app printed "the newest
   version is  " and refused to start. Fixed: unparseable → proceed. Added a 5s HTTP timeout
   (there was none).
4. **Text was pasted twice.** Two independent causes, both fixed: (a) the paste retried a
   keystroke that may already have landed — `osascript` can report failure after delivering
   the keystroke; (b) the processor **appended** every formatted final and joined them, so a
   resent final for the same turn concatenated the utterance with itself. Root cause of (b)
   was that `turn_order` was never plumbed through — the daemon hardcoded `0`.
5. **Dead code removal.** ~21 unreferenced methods, types, and write-only fields, plus a
   metrics/statistics subsystem and a terminal-control package that were removed wholesale.
   `deadcode`, `staticcheck`, `go vet` and a manual zero-caller sweep are all clean.

---

## 5. Known gaps we already suspect

Ranked by our own guess at severity. **Confirm, refute, or re-rank these** — and please treat
them as a starting point, not a boundary.

**5.1 — No backpressure between capture and network.** `audioStreamLoop` calls `SendAudio`
synchronously while the capture loop is the only thing draining the PortAudio buffer, and
`SendAudio` takes `wsMutex` for the write. A slow or stalled network therefore stalls audio
capture. With the 5s write deadline we now fail rather than hang forever, but we likely drop
audio. A buffered channel between capture and send seems obviously right; we haven't done it.
What's the correct shape, and what should happen when that buffer fills?

**5.2 — Cross-session transcript contamination.** The transcript callback runs on the reader
goroutine. `OnRelease` calls `ConsumeTranscriptWithFallback()` then `Reset()`, but a `Turn`
that arrives *after* that `Reset()` lands in the next recording's state. There was a
`sessionActive` field intended to guard exactly this; it was written in three places and never
read, so it was deleted rather than left as decoration. **The gap is real and currently
unguarded.** What's the right fix — a session/generation counter checked in
`ProcessTranscript`, tearing the reader down before consuming, or something else?

**5.3 — The 1-second termination timeout is a guess.** `OnRelease` waits up to 1s for the
server's `Termination` message before falling back to the best partial. Too short loses the
formatted final; too long makes every recording feel sluggish. Is there a principled way to
decide this, or a protocol signal we should be using instead?

**5.4 — Reconnect on every press adds latency.** A WebSocket dial plus a hardcoded 150ms
sleep now sits in front of every recording. The 150ms is superstition — we don't know that
it's needed. Is there a way to keep a warm connection across recordings given AssemblyAI
closes after `Terminate`? Should we pre-dial during idle time?

**5.5 — The connection-health heuristic may be vestigial.** `connectionHealth`,
`failedSessions`, and `ConnectionNeedsRefresh` were built for a long-lived connection. Now
that we reconnect every press, is any of it still earning its place, or is it dead weight that
should follow the rest of the removed code?

**5.6 — The hotkey is Ctrl+Shift with no third key.** Any Ctrl+Shift chord — Ctrl+Shift+T,
Ctrl+Shift+Tab, Ctrl+Shift+arrow for text selection — starts a recording. The 800ms
quick-press filter exists to absorb that. **The author has explicitly chosen to keep this**
for muscle-memory reasons, so don't relitigate the key choice — but if you see a better way to
suppress accidental triggers without changing the chord, that's welcome.

**5.7 — Silently useless on non-macOS.** `detectCtrlShift` returns `false` on any non-darwin
platform, so the binary starts, prints its banner, and never records. It should fail loudly at
startup.

**5.8 — Error handling is lossy.** `handleResponses` has three `return` paths that discard the
error, JSON decode failures `continue` silently, and there's no debug/verbose mode. Diagnosing
the double-paste bug required attaching a test harness because the running tool tells you
almost nothing.

**5.9 — No `context.Context` anywhere.** Shutdown is `stopChan` + `sync.WaitGroup` + a
`done` channel. Is that fine at this size, or is it worth threading context through?

**5.10 — Blocking network call at startup.** `CheckVersion` does an HTTPS GET on every launch
and *hard-blocks the app from running* when a newer version exists. Aggressive for a local
tool; also a startup dependency on GitHub being reachable.

**5.11 — Beeps are synchronous.** `PlayBeep` blocks in `OnPress`/`OnRelease`, adding latency
to the very path we care about.

**5.12 — Clipboard is clobbered with no restore.** Deliberate: the paste-failure path tells
the user the text is on the clipboard so they can press Cmd+V, and any restore would race the
paste and could discard the transcript. Documented here so you can tell us if that reasoning
is wrong.

---

## 6. Places worth a close read

- `internal/transcription/assemblyai.go` — connection lifecycle, who owns `wsConn`, whether
  the reader's identity check is airtight, and whether `wsMutex` is doing too many jobs
  (connection pointer + health counters + serializing writes).
- `internal/audio/recorder.go` — `Start`/`Stop`/`audioStreamLoop`. `Stop` closes `stopChan`
  under the mutex, releases it, then `streamWg.Wait()`, then re-acquires to tear down the
  stream. Is that ordering correct under a concurrent `Cleanup()`? Also: the deferred
  `recover()` shadows the receiver `r`, which is legal but reads badly.
- `internal/hotkeys/simple.go` — the edge-pairing state machine. Buffer is 64; the poller
  blocks rather than drops. Can press/release still desynchronize?
- `internal/app/daemon.go` `OnRelease` — the ordering of stop → gates → terminate → wait →
  consume → reset → paste, and what happens when each step fails.
- `internal/transcription/processor.go` — turn-keyed final assembly and the partial fallback
  heuristic (`confidence > best || len(transcript) > len(best)`), which is unprincipled.

---

## 7. Deliverable

Write your review to **`REVIEW.md` at the repository root.**

If it's useful, order findings so the top of the file is "what I'd fix before using this
daily" and the tail is "what I'd fix if this were going to live for years." The author will
work through them in that order.

Thanks for taking a look.
